package operations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"docker-manager/internal/protocol"
)

type recoveryRecord struct {
	OperationID, Digest, StackID, VersionID, ProjectName, Phase, FinalState string
	LastSeq, ResultSeq, FinalizedSeq                                        uint64
	ClaimsHeld, ReportAcked                                                 bool
	Result, Raw                                                             json.RawMessage
}

func unsupportedRecoveryHistory(r recoveryRecord) bool {
	return r.Phase == "finalized" || r.LastSeq > 2 || r.ResultSeq > 2 || (r.Result != nil && r.LastSeq != r.ResultSeq) || (r.Phase == "START_COMMITTED" && r.LastSeq != 1)
}

func recoveryNumber(n json.Number) (uint64, error) {
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || f < 0 || f > 9007199254740991 || math.Trunc(f) != f {
		return 0, ErrEvidence
	}
	return uint64(f), nil
}

// The caller validates the enclosing shared Schema. This checks relations the
// schema cannot express, retaining the original result instead of re-encoding it.
func parseRecoveryRecord(raw json.RawMessage) (recoveryRecord, error) {
	var in struct {
		OperationID string          `json:"operation_id"`
		Digest      string          `json:"input_digest"`
		StackID     string          `json:"stack_id"`
		VersionID   string          `json:"version_id"`
		ProjectName string          `json:"project_name"`
		Phase       string          `json:"phase"`
		Last        json.Number     `json:"last_event_seq"`
		ResultSeq   *json.Number    `json:"result_event_seq"`
		Result      json.RawMessage `json:"latest_result"`
		Claims      bool            `json:"claims_held"`
		Ack         bool            `json:"report_acked"`
		Final       *string         `json:"final_state"`
		FinalSeq    *json.Number    `json:"finalized_result_event_seq"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return recoveryRecord{}, ErrEvidence
	}
	r := recoveryRecord{OperationID: in.OperationID, Digest: in.Digest, StackID: in.StackID, VersionID: in.VersionID, ProjectName: in.ProjectName, Phase: in.Phase, ClaimsHeld: in.Claims, ReportAcked: in.Ack, Raw: raw}
	var err error
	if r.LastSeq, err = recoveryNumber(in.Last); err != nil {
		return r, err
	}
	if !validID(r.OperationID) || !validID(r.StackID) || !validID(r.VersionID) || !validDigest(r.Digest) {
		return r, ErrEvidence
	}
	if len(in.Result) > 0 && !bytes.Equal(bytes.TrimSpace(in.Result), []byte("null")) {
		r.Result = in.Result
		if in.ResultSeq == nil {
			return r, ErrEvidence
		}
		if r.ResultSeq, err = recoveryNumber(*in.ResultSeq); err != nil || r.ResultSeq == 0 || r.ResultSeq > r.LastSeq {
			return r, ErrEvidence
		}
		p, err := parseEvent(r.Result)
		if err != nil || p.OperationID != r.OperationID || p.Digest != r.Digest || p.seq != r.ResultSeq {
			return r, ErrEvidence
		}
	} else if in.ResultSeq != nil || in.Ack {
		return r, ErrEvidence
	}
	if r.Phase == "finalized" {
		if r.ClaimsHeld || r.Result == nil || in.Final == nil || in.FinalSeq == nil {
			return r, ErrEvidence
		}
		r.FinalState = *in.Final
		if r.FinalizedSeq, err = recoveryNumber(*in.FinalSeq); err != nil || r.FinalizedSeq != r.ResultSeq {
			return r, ErrEvidence
		}
		p, _ := parseEvent(r.Result)
		state, release := resultProjection(p)
		if release != "pending" || state != r.FinalState {
			return r, ErrEvidence
		}
	} else if !r.ClaimsHeld || in.Final != nil || in.FinalSeq != nil {
		return r, ErrEvidence
	}
	switch r.Phase {
	case "preparing", "ready":
		if r.LastSeq != 0 || r.Result != nil {
			return r, ErrEvidence
		}
	case "START_COMMITTED":
		if r.LastSeq < 1 || r.Result != nil {
			return r, ErrEvidence
		}
	case "result":
		if r.Result == nil {
			return r, ErrEvidence
		}
	case "reconciliation_required", "finalized":
	default:
		return r, ErrEvidence
	}
	return r, nil
}

type RecoveryCase struct{ ID, NodeID, LedgerID, OperationID, Kind, RoundID string }

var errRecoveryReleased = errors.New("manager event encountered absence release")

func (s *Store) recoveryQuarantined(ctx context.Context, q querier, node string) (bool, error) {
	s.mu.Lock()
	local := s.recoveryBlocked[node]
	s.mu.Unlock()
	var durable bool
	if err := q.QueryRowContext(ctx, `SELECT quarantined FROM recovery_nodes WHERE node_id=?`, node).Scan(&durable); err != nil {
		return true, ErrEvidence
	}
	return local || durable, nil
}

// The quarantine barrier precedes fallible encryption. If its SQL fails the
// local admission check remains conservative; Open establishes a fresh freeze.
func (s *Store) quarantineRecoveryNode(ctx context.Context, b Binding) error {
	if err := s.requireBinding(ctx, s.db, b, true); err != nil {
		return err
	}
	s.mu.Lock()
	if s.recoveryBlocked == nil {
		s.recoveryBlocked = make(map[string]bool)
	}
	s.recoveryBlocked[b.NodeID] = true
	s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireBinding(ctx, tx, b, true); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_nodes SET quarantined=1 WHERE node_id=?`, b.NodeID); err != nil {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE nodes SET frozen=1 WHERE id=?`, b.NodeID); err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func recoveryCaseBinding(c RecoveryCase) blobBinding {
	context, _ := json.Marshal([]string{c.LedgerID, c.OperationID, c.Kind, c.RoundID})
	return blobBinding{OperationID: c.ID, NodeID: c.NodeID, Digest: string(context), Kind: "recovery_case"}
}

func (s *Store) readRecoveryCase(ctx context.Context, q querier, id string) (RecoveryCase, json.RawMessage, error) {
	var c RecoveryCase
	var blob sealedBlob
	err := q.QueryRowContext(ctx, `SELECT id,node_id,ledger_id,operation_id,kind,round_id,evidence_cipher,wrapped_dek,key_id,fingerprint FROM recovery_cases WHERE id=?`, id).Scan(&c.ID, &c.NodeID, &c.LedgerID, &c.OperationID, &c.Kind, &c.RoundID, &blob.Cipher, &blob.WrappedDEK, &blob.KeyID, &blob.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil, ErrNotFound
	}
	if err != nil {
		return c, nil, ErrStorage
	}
	raw, err := s.unseal(recoveryCaseBinding(c), blob)
	if err != nil {
		return c, nil, err
	}
	if !json.Valid(raw) {
		clear(raw)
		return c, nil, ErrEvidence
	}
	return c, raw, nil
}

// New case encryption reserves its own wrapping use before the business tx.
// The first read avoids spending quota for an already durable identical case.
func (s *Store) putRecoveryCase(ctx context.Context, b Binding, round, operation, kind string, raw []byte) error {
	if len(raw) > maxBlobBytes || !json.Valid(raw) {
		return ErrInput
	}
	if err := s.quarantineRecoveryNode(ctx, b); err != nil {
		return err
	}
	fp, err := s.fingerprint(raw)
	if err != nil {
		return err
	}
	var existing string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM recovery_cases WHERE node_id=? AND operation_id=? AND kind=? AND fingerprint=?`, b.NodeID, operation, kind, fp).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ErrStorage
	}
	var c RecoveryCase
	var sealed sealedBlob
	if existing == "" {
		id, err := protocol.NewID()
		if err != nil {
			return ErrStorage
		}
		c = RecoveryCase{id, b.NodeID, b.LedgerID, operation, kind, round}
		sealed, err = s.seal(ctx, recoveryCaseBinding(c), raw)
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireBinding(ctx, tx, b, true); err != nil {
		return err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM recovery_cases WHERE node_id=? AND operation_id=? AND kind=? AND fingerprint=?`, b.NodeID, operation, kind, fp).Scan(&id)
	if err == nil {
		_, saved, err := s.readRecoveryCase(ctx, tx, id)
		if err != nil {
			return err
		}
		defer clear(saved)
		if !bytes.Equal(saved, raw) {
			return ErrEvidence
		}
	} else if errors.Is(err, sql.ErrNoRows) && c.ID != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO recovery_cases(id,node_id,ledger_id,operation_id,kind,round_id,evidence_cipher,wrapped_dek,key_id,fingerprint) VALUES(?,?,?,?,?,?,?,?,?,?)`, c.ID, c.NodeID, c.LedgerID, c.OperationID, c.Kind, c.RoundID, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint)
		if err != nil {
			return ErrStorage
		}
	} else {
		return ErrStorage
	}
	// Preserve released historical outcomes. A case isolates the node, it is
	// not permission to rewrite an old cancellation or manufacture new claims.
	op, opErr := s.readOperation(ctx, tx, operation)
	if opErr == nil && op.NodeID == b.NodeID && op.LedgerID == b.LedgerID && op.ReleaseStatus != "released" {
		if _, err = tx.ExecContext(ctx, `UPDATE operations SET blocked=1,state='reconciliation_required',release_status='held',revision=revision+CASE WHEN blocked=0 THEN 1 ELSE 0 END,updated_at=CASE WHEN blocked=0 THEN ? ELSE updated_at END WHERE operation_id=?`, time.Now().UnixMilli(), operation); err != nil {
			return ErrStorage
		}
		if _, err = tx.ExecContext(ctx, `UPDATE outbox SET delivery_state='suppressed' WHERE operation_id=? AND kind='finalize'`, operation); err != nil {
			return ErrStorage
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE nodes SET frozen=1 WHERE id=?`, b.NodeID); err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) RecoveryCases(ctx context.Context, node string) ([]RecoveryCase, error) {
	if err := s.requireRecovery(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,node_id,ledger_id,operation_id,kind,round_id FROM recovery_cases WHERE node_id=? ORDER BY id`, node)
	if err != nil {
		return nil, ErrStorage
	}
	defer rows.Close()
	cases := []RecoveryCase{}
	for rows.Next() {
		var c RecoveryCase
		if rows.Scan(&c.ID, &c.NodeID, &c.LedgerID, &c.OperationID, &c.Kind, &c.RoundID) != nil {
			return nil, ErrStorage
		}
		cases = append(cases, c)
	}
	if rows.Err() != nil {
		return nil, ErrStorage
	}
	return cases, nil
}

func (s *Store) ReadRecoveryCase(ctx context.Context, id string) (json.RawMessage, error) {
	if err := s.requireRecovery(); err != nil {
		return nil, err
	}
	_, raw, err := s.readRecoveryCase(ctx, s.db, id)
	return raw, err
}

func (s *Store) ReceiveRecoveryEvent(ctx context.Context, b Binding, raw []byte) ([]byte, error) {
	if err := s.requireRecovery(); err != nil {
		return nil, err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || (env.Type != "started" && env.Type != "result") || env.SessionID == nil || *env.SessionID != b.SessionID {
		return nil, ErrInput
	}
	p, err := parseEvent(env.Body)
	if err != nil {
		return nil, ErrInput
	}
	if err = s.requireBinding(ctx, s.db, b, true); err != nil {
		return nil, err
	}
	op, err := s.Get(ctx, p.OperationID)
	kind := ""
	conflict := false
	if errors.Is(err, ErrNotFound) {
		kind = "unknown_event"
	} else if errors.Is(err, ErrEvidence) {
		kind = "damaged_operation"
	} else if err != nil {
		return nil, err
	} else if op.NodeID != b.NodeID || op.LedgerID != b.LedgerID || op.Digest != p.Digest || (env.Type == "started" && (op.PermitID == "" || op.PermitID != p.PermitID)) || (env.Type == "result" && p.Outcome != "not_started" && op.PermitID == "") {
		kind, conflict = "event_mismatch", true
	} else if op.ReleaseStatus == "released" && ((s.version != 3 && s.version != 4) || op.LatestResultSeq == 0) {
		kind = "released_event"
	} else if !eventPosition(env.Type, p) {
		kind = "unsupported_event"
	}
	if kind != "" {
		if err = s.putRecoveryCase(ctx, b, "", p.OperationID, kind, env.Body); err != nil {
			return nil, err
		}
		if conflict {
			return nil, ErrConflict
		}
		// A durable quarantined event can be acknowledged without inventing a
		// normal operation. It will still prevent completion of the node.
		return protocol.NewMessage("ack", b.SessionID, env.MessageID, map[string]any{"operation_id": p.OperationID, "event_seq": p.seq})
	}
	ack, err := s.receiveNormalEvent(ctx, b, raw)
	if errors.Is(err, errRecoveryReleased) {
		if err = s.putRecoveryCase(ctx, b, "", p.OperationID, "released_event", env.Body); err != nil {
			return nil, err
		}
		return protocol.NewMessage("ack", b.SessionID, env.MessageID, map[string]any{"operation_id": p.OperationID, "event_seq": p.seq})
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrFrozen) || errors.Is(err, ErrEvidence) {
		if caseErr := s.putRecoveryCase(ctx, b, "", p.OperationID, "event_conflict", env.Body); caseErr != nil {
			return nil, caseErr
		}
	}
	return ack, err
}

// Run inside every event write transaction, including the second transaction
// after encryption. A previously received page is evidence even if Process has
// not consumed it yet. No synthetic started event or start timestamp is made.
func (s *Store) validateEventAgainstRecovery(ctx context.Context, q querier, op Operation, kind string, event parsedEvent, body []byte) error {
	rows, err := q.QueryContext(ctx, `SELECT s.id FROM recovery_scans s WHERE s.node_id=? AND s.ledger_id=? AND (EXISTS(SELECT 1 FROM recovery_members m WHERE m.round_id=s.id AND m.operation_id=?) OR EXISTS(SELECT 1 FROM recovery_expected e WHERE e.round_id=s.id AND e.operation_id=?)) ORDER BY s.id`, op.NodeID, op.LedgerID, op.OperationID, op.OperationID)
	if err != nil {
		return ErrStorage
	}
	var rounds []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return ErrStorage
		}
		rounds = append(rounds, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ErrStorage
	}
	for _, id := range rounds {
		scan, err := s.readRecoveryScanByID(ctx, q, id)
		if err != nil {
			return err
		}
		pages, err := s.readRecoveryPages(ctx, q, scan)
		if err != nil {
			return err
		}
		records, err := recoveryRecords(pages)
		clearRecoveryPages(pages)
		if err != nil {
			return err
		}
		checkErr := error(nil)
		for _, record := range records {
			if record.OperationID != op.OperationID {
				continue
			}
			if record.Phase == "reconciliation_required" || s.unsupportedRecoveryHistory(record) {
				checkErr = ErrConflict
				break
			}
			if record.Digest != op.Digest || record.StackID != op.StackID || record.VersionID != op.VersionID || record.ProjectName != op.ProjectName {
				checkErr = ErrConflict
				break
			}
			if kind == "result" && record.Result != nil && record.ResultSeq == event.seq && !bytes.Equal(record.Result, body) {
				checkErr = ErrConflict
				break
			}
			if kind == "result" && event.Outcome == "not_started" && (record.Phase == "START_COMMITTED" || record.ResultSeq > 1) {
				checkErr = ErrConflict
				break
			}
			if record.ResultSeq == 1 && (kind == "started" || kind == "result" && event.Outcome != "not_started") {
				checkErr = ErrConflict
				break
			}
		}
		clearRecoveryRecords(records)
		if checkErr != nil {
			return checkErr
		}
	}
	return nil
}

func (s *Store) unsupportedRecoveryHistory(r recoveryRecord) bool {
	if s.version != 3 && s.version != 4 {
		return unsupportedRecoveryHistory(r)
	}
	return r.LastSeq > 2 || r.ResultSeq > 2 || (r.Result != nil && r.LastSeq != r.ResultSeq) || (r.Phase == "START_COMMITTED" && r.LastSeq != 1)
}
