package operations

import (
	"bytes"
	"context"
	"database/sql"
	"docker-manager/internal/protocol"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

func (s *Store) maxEventSequence() uint64 {
	if s.version == 5 {
		return 9007199254740991
	}
	return 2
}
func (s *Store) eventPosition(kind string, p parsedEvent) bool {
	if s.version != 5 {
		return eventPosition(kind, p)
	}
	return kind == "started" && p.seq == 1 || kind == "result" && (p.Outcome == "not_started" && (p.seq == 1 || p.seq == 2) || p.Outcome != "not_started" && p.seq == 2 || p.Outcome == "normal_exit" && p.seq > 2)
}

// Only observation and ownership may change. Comparing all other schema fields
// also preserves error details, output bytes, interruption reason and timestamps.
func sameProcessFact(a, b []byte) bool {
	var first, second map[string]any
	if json.Unmarshal(a, &first) != nil || json.Unmarshal(b, &second) != nil {
		return false
	}
	for _, key := range []string{"event_seq", "observation", "ownership_status"} {
		delete(first, key)
		delete(second, key)
	}
	return reflect.DeepEqual(first, second)
}
func (s *Store) receiveRuntimeEvent(ctx context.Context, b Binding, raw []byte) ([]byte, error) {
	ack, e := s.ReceiveRecoveryEvent(ctx, b, raw)
	if e != nil {
		return nil, e
	}
	env, _ := s.validator.Validate(raw)
	p, _ := parseEvent(env.Body)
	if e = s.promoteRuntimePending(ctx, b, p.OperationID); e != nil {
		return nil, e
	}
	return ack, nil
}
func (s *Store) promoteRuntimePending(ctx context.Context, b Binding, operation string) error {
	// A higher result may arrive before its predecessor. It was acknowledged only
	// after encrypted persistence; promotion reuses the authenticated event path.
	for count := 0; count < 32; count++ {
		op, e := s.Get(ctx, operation)
		if errors.Is(e, ErrNotFound) || errors.Is(e, ErrEvidence) {
			return nil
		}
		if e != nil {
			return e
		}
		var blob sealedBlob
		seq := op.LatestResultSeq + 1
		e = s.db.QueryRowContext(ctx, `SELECT body_cipher,wrapped_dek,key_id,fingerprint FROM runtime_pending_events WHERE operation_id=? AND event_seq=?`, op.OperationID, seq).Scan(&blob.Cipher, &blob.WrappedDEK, &blob.KeyID, &blob.Fingerprint)
		if errors.Is(e, sql.ErrNoRows) {
			break
		}
		if e != nil {
			return ErrStorage
		}
		body, e := s.unseal(blobBinding{OperationID: op.OperationID, NodeID: op.NodeID, Digest: op.Digest, Kind: "result", Seq: seq}, blob)
		if e != nil {
			return e
		}
		candidate := bodyEnvelope("result", body)
		clear(body)
		candidate = bytes.Replace(candidate, []byte(`"session_id":"`+validationID+`"`), []byte(`"session_id":"`+b.SessionID+`"`), 1)
		_, e = s.ReceiveRecoveryEvent(ctx, b, candidate)
		clear(candidate)
		if e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) receiveRuntimeEventTx(ctx context.Context, tx *sql.Tx, b Binding, op Operation, env protocol.Envelope, p parsedEvent, sealed *sealedBlob) (bool, error) {
	if op.Blocked {
		return false, ErrFrozen
	}
	if env.Type == "started" && (op.PermitID == "" || op.PermitID != p.PermitID) {
		return false, ErrConflict
	}
	if env.Type == "result" && (p.Outcome != "not_started" || p.seq == 2) && op.PermitID == "" {
		return false, ErrConflict
	}
	if p.seq == 1 && env.Type == "result" && op.MaxEventSeq > 0 || p.seq > 1 && op.LatestResultSeq == 1 {
		return false, s.commitEventConflict(ctx, tx, op, p, env.Type, env.Body)
	}
	if env.Type == "result" && p.seq > 2 {
		previous, e := s.readEvent(ctx, tx, op, p.seq-1)
		if errors.Is(e, ErrNotFound) {
			return s.stageRuntimeResult(ctx, tx, op, env, p, sealed)
		}
		if e != nil {
			return false, e
		}
		defer clear(previous.Body)
		previousResult, _ := parseEvent(previous.Body)
		if previous.Kind != "result" || !observationRepairable(previousResult) || p.Outcome != "normal_exit" || !sameProcessFact(previous.Body, env.Body) {
			return false, s.commitEventConflict(ctx, tx, op, p, env.Type, env.Body)
		}
	}
	if env.Type == "result" && op.LatestResultSeq >= p.seq {
		return false, ErrEvidence
	}
	if sealed == nil {
		return true, nil
	}
	if _, e := tx.ExecContext(ctx, `INSERT INTO operation_events(operation_id,source,event_seq,kind,body_cipher,wrapped_dek,key_id,fingerprint,received_at) VALUES(?,'agent',?,?,?,?,?,?,?)`, op.OperationID, p.seq, env.Type, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint, time.Now().UnixMilli()); e != nil {
		return false, ErrStorage
	}
	if _, e := tx.ExecContext(ctx, `DELETE FROM runtime_pending_events WHERE operation_id=? AND event_seq=?`, op.OperationID, p.seq); e != nil {
		return false, ErrStorage
	}
	state, release, resultSeq := op.State, op.ReleaseStatus, op.LatestResultSeq
	if env.Type == "result" {
		state, release = resultProjection(p)
		resultSeq = p.seq
	} else if resultSeq == 0 {
		state = "running"
	}
	if _, e := tx.ExecContext(ctx, `UPDATE operations SET state=?,release_status=?,latest_result_seq=?,max_event_seq=max(max_event_seq,?),revision=revision+1,updated_at=? WHERE operation_id=?`, state, release, resultSeq, p.seq, time.Now().UnixMilli(), op.OperationID); e != nil {
		return false, ErrStorage
	}
	if _, e := tx.ExecContext(ctx, `UPDATE outbox SET delivery_state='confirmed' WHERE operation_id=? AND (kind='prepare' OR (kind='cancel' AND ?='result'))`, op.OperationID, env.Type); e != nil {
		return false, ErrStorage
	}
	if env.Type == "result" && release == "pending" {
		if _, e := tx.ExecContext(ctx, `INSERT INTO outbox(operation_id,kind,generation,delivery_state,result_event_seq,terminal_state) VALUES(?,'finalize',1,'pending',?,?)`, op.OperationID, p.seq, state); e != nil {
			return false, ErrStorage
		}
	}
	if state == "reconciliation_required" {
		if _, e := tx.ExecContext(ctx, `UPDATE nodes SET frozen=1 WHERE id=?`, op.NodeID); e != nil {
			return false, ErrStorage
		}
	}
	if env.Type == "result" {
		if state == "succeeded" {
			if _, e := tx.ExecContext(ctx, `UPDATE stacks SET last_applied_version_id=? WHERE id=? AND node_id=?`, op.VersionID, op.StackID, op.NodeID); e != nil {
				return false, ErrStorage
			}
		}
		if p.Observation != nil {
			if _, e := tx.ExecContext(ctx, `UPDATE stacks SET observation_operation_id=? WHERE id=? AND node_id=?`, op.OperationID, op.StackID, op.NodeID); e != nil {
				return false, ErrStorage
			}
		}
	}
	if e := tx.Commit(); e != nil {
		return false, ErrStorage
	}
	return false, nil
}

func (s *Store) stageRuntimeResult(ctx context.Context, tx *sql.Tx, op Operation, env protocol.Envelope, p parsedEvent, sealed *sealedBlob) (bool, error) {
	var blob sealedBlob
	e := tx.QueryRowContext(ctx, `SELECT body_cipher,wrapped_dek,key_id,fingerprint FROM runtime_pending_events WHERE operation_id=? AND event_seq=?`, op.OperationID, p.seq).Scan(&blob.Cipher, &blob.WrappedDEK, &blob.KeyID, &blob.Fingerprint)
	if e == nil {
		body, e := s.unseal(blobBinding{OperationID: op.OperationID, NodeID: op.NodeID, Digest: op.Digest, Kind: "result", Seq: p.seq}, blob)
		if e != nil {
			return false, e
		}
		defer clear(body)
		if !bytes.Equal(body, env.Body) {
			return false, s.commitEventConflict(ctx, tx, op, p, "result", env.Body)
		}
		if e = tx.Commit(); e != nil {
			return false, ErrStorage
		}
		return false, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return false, ErrStorage
	}
	var n int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM runtime_pending_events WHERE operation_id=?`, op.OperationID).Scan(&n); e != nil {
		return false, ErrStorage
	}
	if n >= 32 {
		return false, ErrRecoveryIncomplete
	}
	if sealed == nil {
		return true, nil
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO runtime_pending_events(operation_id,event_seq,body_cipher,wrapped_dek,key_id,fingerprint) VALUES(?,?,?,?,?,?)`, op.OperationID, p.seq, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint); e != nil {
		return false, ErrStorage
	}
	if e = tx.Commit(); e != nil {
		return false, ErrStorage
	}
	return false, nil
}

func (s *Store) validateRuntimeEvents(ctx context.Context, q querier, op Operation) error {
	if op.ReleaseStatus == "released" && op.LatestResultSeq == 0 {
		return s.verifyAbsenceRelease(ctx, q, op)
	}
	if op.ReleaseStatus == "released" {
		if e := s.verifyRelease(ctx, q, op); e != nil {
			return e
		}
	}
	rows, e := q.QueryContext(ctx, `SELECT event_seq FROM operation_events WHERE operation_id=? ORDER BY event_seq`, op.OperationID)
	if e != nil {
		return ErrStorage
	}
	var seqs []uint64
	for rows.Next() {
		var seq uint64
		if e = rows.Scan(&seq); e != nil {
			rows.Close()
			return ErrStorage
		}
		seqs = append(seqs, seq)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return ErrStorage
	}
	var max, result uint64
	var previous []byte
	defer func() { clear(previous) }()
	state, release := "", "held"
	for _, seq := range seqs {
		event, e := s.readEvent(ctx, q, op, seq)
		if e != nil {
			return e
		}
		max = seq
		if event.Kind == "result" {
			p, _ := parseEvent(event.Body)
			if result > 0 {
				old, _ := parseEvent(previous)
				if result+1 != seq || !observationRepairable(old) || p.Outcome != "normal_exit" || !sameProcessFact(previous, event.Body) {
					clear(event.Body)
					return ErrEvidence
				}
			}
			if seq > 2 && result == 0 {
				clear(event.Body)
				return ErrEvidence
			}
			if seq == 1 && len(seqs) != 1 {
				clear(event.Body)
				return ErrEvidence
			}
			state, release = resultProjection(p)
			result = seq
			clear(previous)
			previous = append([]byte(nil), event.Body...)
		}
		clear(event.Body)
	}
	if max != op.MaxEventSeq || result != op.LatestResultSeq {
		return ErrEvidence
	}
	if op.Blocked {
		if op.State != "reconciliation_required" || op.ReleaseStatus != "held" {
			return ErrEvidence
		}
	} else if result > 0 {
		if state != op.State || (release != op.ReleaseStatus && !(release == "pending" && op.ReleaseStatus == "released")) {
			return ErrEvidence
		}
	} else {
		if op.ReleaseStatus != "held" {
			return ErrEvidence
		}
		if max == 1 {
			if op.State != "running" {
				return ErrEvidence
			}
		} else if op.State != "accepted" && op.State != "ready" && op.State != "authorized" {
			return ErrEvidence
		}
	}
	var intentState, terminal string
	var intentSeq uint64
	e = q.QueryRowContext(ctx, `SELECT delivery_state,result_event_seq,terminal_state FROM outbox WHERE operation_id=? AND kind='finalize' AND generation=1`, op.OperationID).Scan(&intentState, &intentSeq, &terminal)
	if release == "pending" && result > 0 {
		want := "pending"
		if op.ReleaseStatus == "released" {
			want = "confirmed"
		}
		if op.Blocked {
			want = "suppressed"
		}
		if e != nil || intentState != want || intentSeq != result || terminal != state {
			return ErrEvidence
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ErrEvidence
	}
	return nil
}
