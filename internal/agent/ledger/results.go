package ledger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"time"
)

// RecordResultFixture stores a controlled result supplied by its caller. It
// validates ledger prerequisites, not a real process exit or Docker ownership.
func (s *Store) RecordResultFixture(ctx context.Context, rawResult []byte) (Event, error) {
	if err := s.requireJournal(); err != nil {
		return Event{}, err
	}
	env, err := s.validator.Validate(rawResult)
	if err != nil || env.Type != "result" || env.SessionID == nil || len(env.Body) > maxEventBodyBytes {
		return Event{}, ErrInput
	}
	binding, err := eventBodyBinding(env.Body, "result")
	if err != nil {
		return Event{}, ErrInput
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return Event{}, ErrStorage
	}
	key := append([]byte(nil), s.key...)
	s.mu.Unlock()
	mac := hmac.New(sha256.New, key)
	mac.Write(env.Body)
	fingerprint := mac.Sum(nil)
	clear(key)
	// The first transaction handles a visible replay/conflict without reserving
	// another encryption use. All new-write predicates run again after sealing.
	event, needsSeal, err := s.recordResult(ctx, binding, env.Body, *env.SessionID, fingerprint, nil)
	if err != nil || !needsSeal {
		return event, err
	}
	sealed, err := s.sealEvent(ctx, binding, env.Body)
	if err != nil {
		return Event{}, err
	}
	event, _, err = s.recordResult(ctx, binding, env.Body, *env.SessionID, fingerprint, &sealed)
	return event, err
}

func (s *Store) recordResult(ctx context.Context, b eventBinding, body []byte, session string, fingerprint []byte, sealed *sealedEvent) (Event, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, ErrStorage
	}
	defer tx.Rollback()
	if err := s.requireRecoveryWriter(ctx, tx); err != nil {
		return Event{}, false, err
	}
	record, err := s.readRecord(ctx, tx, b.OperationID)
	if err != nil {
		return Event{}, false, err
	}
	if record.Digest != b.Digest {
		return Event{}, false, ErrConflict
	}
	if err := requireEventClaims(ctx, tx, record); err != nil {
		return Event{}, false, err
	}
	state, err := readEventState(ctx, tx, b.OperationID)
	if err != nil {
		return Event{}, false, err
	}
	if b.Seq <= state.LastSeq {
		existing, err := s.readEvent(ctx, tx, b.OperationID, b.Seq)
		if err != nil {
			return Event{}, false, err
		}
		if existing.Kind != "result" || !state.LatestResultSeq.Valid || uint64(state.LatestResultSeq.Int64) != b.Seq {
			return Event{}, false, ErrConflict
		}
		if bytes.Equal(existing.Body, body) {
			return existing, false, nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_conflicts(operation_id,event_seq,body_fingerprint,created_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, b.OperationID, b.Seq, fingerprint, time.Now().UnixMilli()); err != nil {
			return Event{}, false, ErrStorage
		}
		if _, err := tx.ExecContext(ctx, `UPDATE event_state SET blocked=1 WHERE operation_id=?`, b.OperationID); err != nil {
			return Event{}, false, ErrStorage
		}
		if err := s.noteRecoveryConflict(ctx, tx); err != nil {
			return Event{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return Event{}, false, ErrStorage
		}
		return Event{}, false, ErrConflict
	}
	if state.Blocked || state.LatestResultSeq.Valid || b.Seq != state.LastSeq+1 {
		return Event{}, false, ErrConflict
	}
	if record.BootID != s.boot {
		return Event{}, false, ErrFrozen
	}
	if record.PreparedSession != session {
		return Event{}, false, ErrConflict
	}
	var fields struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return Event{}, false, ErrInput
	}
	switch record.Phase {
	case PhaseReady:
		if fields.Outcome != "not_started" || state.LastSeq != 0 {
			return Event{}, false, ErrConflict
		}
	case PhaseStartCommitted:
		if fields.Outcome == "not_started" || state.LastSeq != 1 {
			return Event{}, false, ErrConflict
		}
		if err := s.requireStartedEvent(ctx, tx, record); err != nil {
			return Event{}, false, err
		}
	default:
		return Event{}, false, ErrConflict
	}
	if sealed == nil {
		return Event{}, true, nil
	}
	if err := s.insertEvent(ctx, tx, b, *sealed); err != nil {
		return Event{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_state SET last_seq=?,latest_result_seq=? WHERE operation_id=?`, b.Seq, b.Seq, b.OperationID); err != nil {
		return Event{}, false, ErrStorage
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, ErrStorage
	}
	return Event{OperationID: b.OperationID, Digest: b.Digest, Kind: b.Kind, Seq: b.Seq, Body: append(json.RawMessage(nil), body...)}, false, nil
}

func (s *Store) insertEvent(ctx context.Context, tx *sql.Tx, b eventBinding, sealed sealedEvent) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_events(operation_id,event_seq,kind,body_cipher,wrapped_dek,key_id,body_fingerprint) VALUES(?,?,?,?,?,?,?)`, b.OperationID, b.Seq, b.Kind, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint); err != nil {
		return ErrStorage
	}
	return s.noteRecoveryEvent(ctx, tx, b.OperationID, b.Seq)
}

func (s *Store) requireStartedEvent(ctx context.Context, q querier, record Record) error {
	started, err := s.readEvent(ctx, q, record.OperationID, 1)
	if err != nil {
		return ErrEvidence
	}
	var body struct {
		PermitID  string `json:"permit_id"`
		StartedAt string `json:"started_at"`
	}
	if started.Kind != "started" || record.Permit == nil || json.Unmarshal(started.Body, &body) != nil || body.PermitID != record.Permit.PermitID || body.StartedAt != record.StartedAt {
		return ErrEvidence
	}
	return nil
}
