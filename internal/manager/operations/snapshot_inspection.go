package operations

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"docker-manager/internal/protocol"
	"docker-manager/internal/snapshotcheck"
)

var ErrInspectionLimit = errors.New("manager inspection limit exceeded")

// SnapshotInspection is a complete, secret-free report for one closed v5 copy.
type SnapshotInspection struct {
	Identity            Identity
	SchemaVersion       int
	ProtocolVersion     int
	EncryptionKeyID     string
	EncryptionKeyUses   uint64
	TableRows           map[string]uint64
	SealedRows          map[string]uint64
	SemanticFormat      string
	SemanticFingerprint string
}

// InspectRuntimeSnapshot checks a closed snapshot without starting a service.
func InspectRuntimeSnapshot(ctx context.Context, path string, expected Identity, keys Keys) (SnapshotInspection, error) {
	if ctx == nil {
		return SnapshotInspection{}, ErrInput
	}
	if !validID(expected.ManagerID) || !validKeyID(expected.DigestKeyID) || !validKeyID(expected.EncryptionKeyID) {
		return SnapshotInspection{}, ErrIdentity
	}
	if err := validateKeys(keys); err != nil {
		return SnapshotInspection{}, err
	}
	if expected.DigestKeyID != keys.DigestID || expected.EncryptionKeyID != keys.EncryptionID {
		return SnapshotInspection{}, ErrKey
	}
	reader, err := snapshotcheck.Open(ctx, path)
	if err != nil {
		return SnapshotInspection{}, inspectionError(err)
	}
	defer reader.Close()
	validator, err := protocol.NewValidator()
	if err != nil {
		return SnapshotInspection{}, ErrSchema
	}
	// This evidence-only view has no DB, generated boot, startup or writer handle.
	view := &Store{version: 5, identity: expected, keys: Keys{keys.DigestID, keys.EncryptionID, append([]byte(nil), keys.DigestKey...), append([]byte(nil), keys.EncryptionKey...)}, validator: validator}
	defer clear(view.keys.DigestKey)
	defer clear(view.keys.EncryptionKey)
	i := &snapshotInspector{ctx: reader.Context(), tx: reader.Tx(), view: view}
	if err = i.header(expected); err != nil {
		return SnapshotInspection{}, i.failure(err)
	}
	fp, err := snapshotcheck.NewFingerprint(view.keys.DigestKey, "manager", "dm-manager-runtime-semantic-v1", []string{expected.ManagerID, keys.DigestID}, 5, 1)
	if err != nil {
		return SnapshotInspection{}, inspectionError(err)
	}
	defer fp.Close()
	report := SnapshotInspection{Identity: expected, SchemaVersion: 5, ProtocolVersion: 1, EncryptionKeyID: keys.EncryptionID, EncryptionKeyUses: i.uses, TableRows: map[string]uint64{}, SealedRows: map[string]uint64{}, SemanticFormat: "dm-manager-runtime-semantic-v1"}
	var sealed uint64
	for _, table := range inspectionTables {
		if err = i.ctx.Err(); err != nil {
			return SnapshotInspection{}, err
		}
		n, err := i.table(table, fp)
		if err != nil {
			return SnapshotInspection{}, i.failure(err)
		}
		report.TableRows[table.name] = n
		if table.body != "" {
			report.SealedRows[table.name] = n
			sealed += n
		}
	}
	if sealed > i.uses {
		return SnapshotInspection{}, ErrEvidence
	}
	fp.Marker("database-end")
	if err = fp.Value(uint64(len(inspectionTables))); err != nil {
		return SnapshotInspection{}, inspectionError(err)
	}
	report.SemanticFingerprint = fp.Sum()
	if err = reader.Finish(); err != nil {
		return SnapshotInspection{}, inspectionError(err)
	}
	return report, nil
}

func inspectionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, ErrInspectionLimit), errors.Is(err, snapshotcheck.ErrLimit):
		return ErrInspectionLimit
	case errors.Is(err, ErrIdentity):
		return ErrIdentity
	case errors.Is(err, ErrKey):
		return ErrKey
	case errors.Is(err, ErrSchema):
		return ErrSchema
	case errors.Is(err, snapshotcheck.ErrInput), errors.Is(err, ErrInput):
		return ErrInput
	case errors.Is(err, snapshotcheck.ErrChanged), errors.Is(err, ErrEvidence), errors.Is(err, ErrNotFound), errors.Is(err, ErrRecovery), errors.Is(err, ErrRecoveryIncomplete), errors.Is(err, ErrFrozen), errors.Is(err, ErrConflict):
		return ErrEvidence
	default:
		return ErrStorage
	}
}

type inspectionTable struct{ name, columns, pk, body string }

// This independent registry is the completeness boundary, including clear tables.
var inspectionTables = []inspectionTable{
	{"manager_meta", "singleton,schema_version,manager_id,active_boot,recovery_mode,initialization_complete", "singleton", ""},
	{"keys", "singleton,digest_key_id,encryption_key_id,digest_key_check,encryption_key_check,uses", "singleton", ""},
	{"users", "id,enabled,auth_rev,login_name,password_hash,system_admin,created_at", "id", ""},
	{"nodes", "id,ledger_id,session_id,boot_id,frozen,display_name,credential_generation,revoked_at,created_at,last_seen_at,agent_capabilities_json,execution_profile_json", "id", ""},
	{"grants", "user_id,node_id,capability", "user_id,node_id,capability", ""},
	{"operations", "operation_id,idempotency_key,digest_key_id,user_id,node_id,ledger_id,stack_id,version_id,project_name,endpoint_id,policy_revision,input_digest,owner_boot,prepared_session,payload_cipher,wrapped_dek,key_id,fingerprint,state,revision,cancel_requested,permit_id,permit_session,start_nonce,auth_rev,latest_result_seq,max_event_seq,release_status,blocked,created_at,updated_at", "operation_id", "payload_cipher"},
	{"resource_claims", "node_id,resource_kind,resource_id,operation_id", "node_id,resource_kind,resource_id", ""},
	{"outbox", "operation_id,kind,generation,delivery_state,result_event_seq,terminal_state", "operation_id,kind,generation", ""},
	{"operation_events", "operation_id,source,event_seq,kind,body_cipher,wrapped_dek,key_id,fingerprint,received_at", "operation_id,source,event_seq", "body_cipher"},
	{"event_conflicts", "operation_id,event_seq,kind,fingerprint,created_at", "operation_id,event_seq,kind,fingerprint", ""},
	{"recovery_nodes", "node_id,agent_boot,quarantined,active_round", "node_id", ""},
	{"recovery_scans", "id,node_id,ledger_id,session_id,manager_boot,agent_boot,scope,page_limit,scan_id,status,total,seen,pages,body_bytes,next_cursor,last_operation", "id", ""},
	{"recovery_requests", "message_id,round_id,kind,page_no,cursor,raw,accepted", "message_id", ""},
	{"recovery_pages", "round_id,page_no,request_id,body_cipher,wrapped_dek,key_id,fingerprint", "round_id,page_no", "body_cipher"},
	{"recovery_members", "round_id,ordinal,operation_id,phase,page_no,record_no", "round_id,ordinal", ""},
	{"recovery_cases", "id,node_id,ledger_id,operation_id,kind,round_id,evidence_cipher,wrapped_dek,key_id,fingerprint", "id", "evidence_cipher"},
	{"absence_releases", "operation_id,round_id", "operation_id", ""},
	{"recovery_expected", "round_id,operation_id,last_event_seq", "round_id,operation_id", ""},
	{"finalize_requests", "message_id,operation_id,node_id,ledger_id,session_id,manager_boot,agent_boot,round_id,raw,accepted,confirmation_mac", "message_id", ""},
	{"finalization", "operation_id,request_id,result_event_seq,terminal_state,body_cipher,wrapped_dek,key_id,fingerprint", "operation_id", "body_cipher"},
	{"user_sessions", "id,token_hash,user_id,auth_rev,created_at,last_seen_at,expires_at,revoked_at,csrf_cipher,wrapped_dek,key_id,fingerprint", "token_hash", "csrf_cipher"},
	{"node_credentials", "node_id,generation,token_hash,enabled,created_at", "node_id,generation", ""},
	{"enrollments", "id,token_hash,node_id,ledger_id,expires_at,consumed_at,candidate_hash,revoked_at,created_at", "id", ""},
	{"stacks", "id,node_id,project_name,display_name,current_version_id,revision,last_applied_version_id,last_operation_id,observation_operation_id,created_at,disabled_at", "id", ""},
	{"config_versions", "id,stack_id,parent_id,created_by,created_at,package_cipher,key_id,wrapped_dek,fingerprint", "id", "package_cipher"},
	{"idempotency_records", "user_id,idempotency_key,method,path,request_digest,digest_key_id,result_type,result_id,response_status,safe_response_json,response_etag,response_location,created_at", "user_id,idempotency_key", ""},
	{"audit_events", "id,user_id,node_id,action,target_type,target_id,operation_id,created_at,outcome", "id", ""},
	{"runtime_deliveries", "message_id,operation_id,node_id,ledger_id,session_id,manager_boot,round_id,kind,generation,body_cipher,wrapped_dek,key_id,fingerprint,attempts,next_attempt,failure_code", "message_id", "body_cipher"},
	{"runtime_pending_events", "operation_id,event_seq,body_cipher,wrapped_dek,key_id,fingerprint", "operation_id,event_seq", "body_cipher"},
}

type snapshotInspector struct {
	ctx  context.Context
	tx   *sql.Tx
	view *Store
	uses uint64
}

func (i *snapshotInspector) failure(err error) error {
	if e := i.ctx.Err(); e != nil {
		return e
	}
	return inspectionError(err)
}

type inspectionRow map[string]any

func (r inspectionRow) s(k string) string { v, _ := r[k].(string); return v }
func (r inspectionRow) n(k string) int64  { v, _ := r[k].(int64); return v }
func (r inspectionRow) b(k string) []byte { v, _ := r[k].([]byte); return v }
func (r inspectionRow) blob(body string) sealedBlob {
	return sealedBlob{r.b(body), r.b("wrapped_dek"), r.b("fingerprint"), r.s("key_id")}
}
func (i *snapshotInspector) header(expected Identity) error {
	var version int
	if err := i.tx.QueryRowContext(i.ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return ErrStorage
	}
	if version != 5 {
		return ErrSchema
	}
	if err := validateSchemaQuery(i.ctx, i.tx, schemaV5); err != nil {
		return err
	}
	var count, complete, admins, sv int
	var manager, boot string
	if err := i.tx.QueryRowContext(i.ctx, `SELECT count(*),schema_version,manager_id,active_boot,initialization_complete,(SELECT count(*) FROM users WHERE enabled=1 AND system_admin=1) FROM manager_meta`).Scan(&count, &sv, &manager, &boot, &complete, &admins); err != nil {
		return ErrSchema
	}
	if count != 1 || sv != 5 || complete != 1 || admins < 1 || !validID(boot) {
		return ErrSchema
	}
	if manager != expected.ManagerID {
		return ErrIdentity
	}
	var digestID, encID string
	var d, e []byte
	var uses int64
	if err := i.tx.QueryRowContext(i.ctx, `SELECT count(*),digest_key_id,encryption_key_id,digest_key_check,encryption_key_check,uses FROM keys`).Scan(&count, &digestID, &encID, &d, &e, &uses); err != nil {
		return ErrKey
	}
	k := i.view.keys
	if count != 1 || digestID != k.DigestID || encID != k.EncryptionID || !hmac.Equal(d, keyCheck(manager, "dm-manager-digest-key-v1", digestID, k.DigestKey)) || !hmac.Equal(e, keyCheck(manager, "dm-manager-encryption-key-v1", encID, k.EncryptionKey)) {
		return ErrKey
	}
	if uses < 0 || uses > maxKeyUses {
		return ErrEvidence
	}
	i.uses = uint64(uses)
	rows, err := i.tx.QueryContext(i.ctx, `PRAGMA integrity_check`)
	if err != nil {
		return ErrStorage
	}
	n := 0
	for rows.Next() {
		var v string
		if rows.Scan(&v) != nil || v != "ok" {
			rows.Close()
			return ErrEvidence
		}
		n++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ErrStorage
	}
	if n != 1 {
		return ErrEvidence
	}
	rows, err = i.tx.QueryContext(i.ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return ErrStorage
	}
	bad := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ErrStorage
	}
	if bad {
		return ErrEvidence
	}
	return nil
}

// One key at a time bounds both row memory and nested-query connection use.
func (i *snapshotInspector) table(t inspectionTable, fp *snapshotcheck.Fingerprint) (uint64, error) {
	cols := strings.Split(t.columns, ",")
	pks := strings.Split(t.pk, ",")
	// Check the complete column inventory as well as the exact supported DDL.
	rows, err := i.tx.QueryContext(i.ctx, "PRAGMA table_info("+t.name+")")
	if err != nil {
		return 0, ErrStorage
	}
	var actual []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			rows.Close()
			return 0, ErrStorage
		}
		actual = append(actual, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, ErrStorage
	}
	if strings.Join(actual, ",") != t.columns {
		return 0, ErrSchema
	}
	fp.Marker("table")
	if err = fp.Value(t.name); err != nil {
		return 0, err
	}
	for _, c := range cols {
		if inspectionExcluded(t.name, c) {
			continue
		}
		fp.Marker("column")
		if err = fp.Value(c); err != nil {
			return 0, err
		}
	}
	var last []any
	var n uint64
	for {
		if err = i.ctx.Err(); err != nil {
			return 0, err
		}
		where := ""
		if last != nil {
			where = " WHERE (" + t.pk + ")>(" + strings.TrimSuffix(strings.Repeat("?,", len(pks)), ",") + ")"
		}
		var lengths []string
		for _, c := range cols {
			lengths = append(lengths, "CASE WHEN typeof("+c+") IN ('text','blob') THEN length(CAST("+c+" AS BLOB)) ELSE 8 END")
		}
		query := "SELECT " + t.pk + "," + strings.Join(lengths, ",") + " FROM " + t.name + where + " ORDER BY " + t.pk + " LIMIT 1"
		key := make([]any, len(pks))
		lens := make([]int64, len(cols))
		dest := make([]any, 0, len(pks)+len(cols))
		for j := range key {
			dest = append(dest, &key[j])
		}
		for j := range lens {
			dest = append(dest, &lens[j])
		}
		err = i.tx.QueryRowContext(i.ctx, query, last...).Scan(dest...)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return 0, inspectionError(snapshotcheck.StorageError(i.ctx, err))
		}
		var bytes int64
		for j, v := range lens {
			limit := int64(32 << 20)
			if cols[j] == t.body || cols[j] == "raw" {
				limit = 16 << 20
			}
			if v < 0 || v > limit {
				return 0, ErrInspectionLimit
			}
			bytes += v
		}
		if bytes > 32<<20 {
			return 0, ErrInspectionLimit
		}
		vals := make([]any, len(cols))
		dest = dest[:0]
		for j := range vals {
			dest = append(dest, &vals[j])
		}
		equal := make([]string, len(pks))
		for j, c := range pks {
			equal[j] = c + "=?"
		}
		if err = i.tx.QueryRowContext(i.ctx, "SELECT "+t.columns+" FROM "+t.name+" WHERE "+strings.Join(equal, " AND "), key...).Scan(dest...); err != nil {
			return 0, inspectionError(snapshotcheck.StorageError(i.ctx, err))
		}
		row := inspectionRow{}
		for j, c := range cols {
			row[c] = vals[j]
			if s, ok := vals[j].(string); ok && !utf8.ValidString(s) {
				return 0, ErrEvidence
			}
		}
		raw, err := i.row(t, row)
		if err != nil {
			clear(raw)
			return 0, err
		}
		fp.Marker("row")
		for _, c := range cols {
			if inspectionExcluded(t.name, c) {
				continue
			}
			v := row[c]
			if c == t.body {
				v = raw
			}
			if err = fp.Value(v); err != nil {
				clear(raw)
				return 0, err
			}
		}
		clear(raw)
		for _, v := range vals {
			if b, ok := v.([]byte); ok {
				clear(b)
			}
		}
		// Keys can be BLOBs; retain independent bytes before clearing row buffers.
		last = make([]any, len(key))
		for j, v := range key {
			if b, ok := v.([]byte); ok {
				last[j] = append([]byte(nil), b...)
			} else {
				last[j] = v
			}
		}
		n++
	}
	fp.Marker("table-end")
	if err = fp.Value(n); err != nil {
		return 0, err
	}
	return n, nil
}
func inspectionExcluded(table, column string) bool {
	if table == "keys" && (column == "encryption_key_id" || column == "encryption_key_check" || column == "uses") {
		return true
	}
	return column == "wrapped_dek" || column == "key_id"
}
func (i *snapshotInspector) count(query string, args ...any) (int64, error) {
	var n int64
	if err := i.tx.QueryRowContext(i.ctx, query, args...).Scan(&n); err != nil {
		return 0, ErrStorage
	}
	return n, nil
}
func (i *snapshotInspector) assert(query string, args ...any) error {
	n, err := i.count(query, args...)
	if err != nil {
		return err
	}
	if n != 0 {
		return ErrEvidence
	}
	return nil
}
func inspectionID(v string, empty bool) bool { return empty && v == "" || validID(v) }
