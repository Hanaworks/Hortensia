package ledger

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"docker-manager/internal/snapshotcheck"
)

const inspectionBodyLimit = 16 << 20
const inspectionRowLimit = 32 << 20

var inspectionTables = []string{"agent_meta", "event_keys", "release_control", "recovery_control", "runtime_profile", "executions", "local_claims", "event_state", "execution_events", "event_conflicts", "finalizations", "recovery_members", "recovery_changes", "runtime_documents", "process_facts", "project_bindings", "observation_queries"}

type inspectionColumn struct {
	name, typ string
	pk        int
}

func inspectionColumns(ctx context.Context, q querier, table string) ([]inspectionColumn, error) {
	rows, err := q.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return nil, ErrStorage
	}
	var cols []inspectionColumn
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if rows.Scan(&cid, &name, &typ, &nn, &def, &pk) != nil {
			rows.Close()
			return nil, ErrStorage
		}
		cols = append(cols, inspectionColumn{name, typ, pk})
	}
	e, c := rows.Err(), rows.Close()
	if e != nil || c != nil {
		return nil, ErrStorage
	}
	return cols, nil
}

func inspectionWalk(ctx context.Context, q querier, table string, cols []inspectionColumn, visit func([]any) error) error {
	names := make([]string, len(cols))
	var keys []string
	for i, c := range cols {
		names[i] = `"` + c.name + `"`
	}
	for n := 1; ; n++ {
		found := false
		for _, c := range cols {
			if c.pk == n {
				keys = append(keys, `"`+c.name+`"`)
				found = true
			}
		}
		if !found {
			break
		}
	}
	if len(keys) == 0 {
		return ErrSchema
	}
	var after []any
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		query := `SELECT ` + strings.Join(names, ",") + ` FROM "` + table + `"`
		args := []any{}
		if after != nil {
			query += ` WHERE (` + strings.Join(keys, ",") + `) > (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)`
			args = after
		}
		query += ` ORDER BY ` + strings.Join(keys, ",") + ` LIMIT 1`
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return ErrStorage
		}
		batch := [][]any{}
		for rows.Next() {
			v := make([]any, len(cols))
			ptr := make([]any, len(cols))
			for i := range v {
				ptr[i] = &v[i]
			}
			if rows.Scan(ptr...) != nil {
				rows.Close()
				return ErrStorage
			}
			batch = append(batch, v)
		}
		e, c := rows.Err(), rows.Close()
		if e != nil || c != nil {
			return ErrStorage
		}
		for _, v := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(v); err != nil {
				return err
			}
		}
		if len(batch) == 0 {
			return nil
		}
		last := batch[len(batch)-1]
		after = make([]any, len(keys))
		for i, c := range cols {
			if c.pk > 0 {
				after[c.pk-1] = last[i]
			}
		}
	}
}

func inspectionPreflight(ctx context.Context, q querier, table string, cols []inspectionColumn) error {
	var lengths []string
	for _, c := range cols {
		expr := `coalesce(length(CAST("` + c.name + `" AS BLOB)),0)`
		lengths = append(lengths, expr)
		limit := inspectionRowLimit
		if c.name == "body_cipher" {
			limit = inspectionBodyLimit + 28
		}
		var n int
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "`+table+`" WHERE `+expr+`>?)`, limit).Scan(&n); err != nil {
			return inspectionReaderError(ctx, snapshotcheck.StorageError(ctx, err))
		}
		if n != 0 {
			return ErrInspectionLimit
		}
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "`+table+`" WHERE `+strings.Join(lengths, "+")+`>?)`, inspectionRowLimit).Scan(&n); err != nil {
		return inspectionReaderError(ctx, snapshotcheck.StorageError(ctx, err))
	}
	if n != 0 {
		return ErrInspectionLimit
	}
	return nil
}

func (s *Store) inspectRows(ctx context.Context, tx *sql.Tx, fp *snapshotcheck.Fingerprint, report *SnapshotInspection) error {
	for _, table := range inspectionTables {
		report.TableRows[table] = 0
		cols, err := inspectionColumns(ctx, tx, table)
		if err != nil {
			return err
		}
		if err = inspectionPreflight(ctx, tx, table, cols); err != nil {
			return err
		}
		fp.Marker("table:" + table)
		for _, c := range cols {
			if table == "event_keys" && (c.name == "key_id" || c.name == "key_check" || c.name == "uses") {
				continue
			}
			if c.name == "wrapped_dek" || c.name == "key_id" && table != "agent_meta" {
				continue
			}
			fp.Marker("column:" + c.name + ":" + c.typ)
		}
		err = inspectionWalk(ctx, tx, table, cols, func(v []any) error {
			report.TableRows[table]++
			fp.Marker("row")
			m := map[string]any{}
			for i, c := range cols {
				m[c.name] = v[i]
				if value, ok := v[i].(string); ok && !utf8.ValidString(value) {
					return ErrEvidence
				}
			}
			var plain []byte
			if table == "execution_events" || table == "finalizations" || table == "runtime_documents" {
				r, e := s.readRecord(ctx, tx, m["operation_id"].(string))
				if e != nil {
					return inspectionEvidence(e)
				}
				var kind string
				var seq uint64
				switch table {
				case "execution_events":
					kind = m["kind"].(string)
					seq = uint64(m["event_seq"].(int64))
				case "finalizations":
					kind = "finalized"
					seq = uint64(m["result_event_seq"].(int64))
				case "runtime_documents":
					kind = "runtime-" + m["kind"].(string)
					seq = uint64(m["seq"].(int64))
				}
				plain, e = s.openEvent(eventBinding{r.OperationID, r.Digest, kind, seq}, sealedEvent{Cipher: m["body_cipher"].([]byte), WrappedDEK: m["wrapped_dek"].([]byte), KeyID: m["key_id"].(string), Fingerprint: m["body_fingerprint"].([]byte)})
				if e != nil {
					return e
				}
				defer clear(plain)
				if len(plain) > inspectionBodyLimit {
					return ErrInspectionLimit
				}
				report.SealedRows[table]++
				if table == "runtime_documents" {
					if e = inspectionDocumentShape(m["kind"].(string), plain); e != nil {
						return e
					}
				}
			}
			for i, c := range cols {
				if table == "event_keys" && (c.name == "key_id" || c.name == "key_check" || c.name == "uses") {
					continue
				}
				if c.name == "wrapped_dek" || c.name == "key_id" && table != "agent_meta" {
					continue
				}
				value := v[i]
				if c.name == "body_cipher" {
					value = plain
				}
				if err := fp.Value(value); err != nil {
					return ErrEvidence
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		fp.Marker("rows")
		if err = fp.Value(report.TableRows[table]); err != nil {
			return ErrEvidence
		}
	}
	return nil
}

func inspectionEvidence(err error) error {
	if err == ErrStorage {
		return err
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return err
	}
	return ErrEvidence
}

func inspectionCount(ctx context.Context, q querier, query string, args ...any) (uint64, error) {
	var n uint64
	if q.QueryRowContext(ctx, query, args...).Scan(&n) != nil {
		return 0, ErrStorage
	}
	return n, nil
}
func inspectionExpect(ctx context.Context, q querier, want uint64, query string, args ...any) error {
	n, err := inspectionCount(ctx, q, query, args...)
	if err != nil {
		return err
	}
	if n != want {
		return ErrEvidence
	}
	return nil
}
