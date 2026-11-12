package operations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"docker-manager/internal/protocol"
)

const maxEventBodyBytes = 7 << 20
const validationID = "00000000-0000-4000-8000-000000000000"

type parsedEvent struct {
	OperationID string       `json:"operation_id"`
	Digest      string       `json:"input_digest"`
	EventSeq    json.Number  `json:"event_seq"`
	PermitID    string       `json:"permit_id"`
	Outcome     string       `json:"outcome"`
	Ownership   string       `json:"ownership_status"`
	ExitCode    *json.Number `json:"exit_code"`
	Observation *struct {
		Status string `json:"status"`
	} `json:"observation"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
	seq uint64
}

// parseEvent runs only after full protocol validation. Conversion therefore
// preserves the exact value of all permitted mathematical safe integers.
func parseEvent(body []byte) (parsedEvent, error) {
	var p parsedEvent
	if err := json.Unmarshal(body, &p); err != nil {
		return p, ErrInput
	}
	n, err := p.EventSeq.Float64()
	if err != nil || n < 1 || n > 9007199254740991 || n != float64(uint64(n)) {
		return p, ErrInput
	}
	p.seq = uint64(n)
	return p, nil
}

// bodyEnvelope attaches a validation-only envelope without re-encoding the
// evidence body. RawMessage passed to json.Marshal would change its bytes.
func bodyEnvelope(kind string, body []byte) []byte {
	head, _ := json.Marshal(struct {
		Protocol  int    `json:"protocol"`
		Type      string `json:"type"`
		MessageID string `json:"message_id"`
		SessionID string `json:"session_id"`
		ReplyTo   any    `json:"reply_to"`
	}{1, kind, validationID, validationID, nil})
	raw := append(head[:len(head)-1], []byte(`,"body":`)...)
	raw = append(raw, body...)
	return append(raw, '}')
}

func eventPosition(kind string, p parsedEvent) bool {
	return (kind == "started" && p.seq == 1) || (kind == "result" && ((p.Outcome == "not_started" && p.seq == 1) || (p.Outcome != "not_started" && p.seq == 2)))
}

func resultProjection(p parsedEvent) (state, release string) {
	if p.Outcome == "not_started" {
		if p.Error != nil && p.Error.Code == "canceled_before_start" {
			return "canceled", "pending"
		}
		return "failed", "pending"
	}
	if p.Outcome == "normal_exit" && p.Observation != nil && p.Observation.Status == "complete" && p.Ownership == "consistent" && p.ExitCode != nil {
		n, err := p.ExitCode.Float64()
		if err == nil {
			if n == 0 {
				return "succeeded", "pending"
			}
			return "failed", "pending"
		}
	}
	return "reconciliation_required", "held"
}

// ReceiveEvent validates a trusted connection binding and durable evidence. It
// does not authenticate a socket or prove that the reported Docker work ran.
// The returned ack may be sent only after this call succeeds.
func (s *Store) ReceiveEvent(ctx context.Context, b Binding, raw []byte) ([]byte, error) {
	if s.version == 5 {
		return s.receiveRuntimeEvent(ctx, b, raw)
	}
	if s.version == 2 || s.version == 3 || (s.version == 4 || s.version == 5) {
		return s.ReceiveRecoveryEvent(ctx, b, raw)
	}
	return s.receiveNormalEvent(ctx, b, raw)
}

func (s *Store) receiveNormalEvent(ctx context.Context, b Binding, raw []byte) ([]byte, error) {
	env, err := s.validator.Validate(raw)
	if err != nil || (env.Type != "started" && env.Type != "result") || env.SessionID == nil || *env.SessionID != b.SessionID || len(env.Body) > maxEventBodyBytes {
		return nil, ErrInput
	}
	p, err := parseEvent(env.Body)
	if err != nil || !s.eventPosition(env.Type, p) {
		return nil, ErrInput
	}
	needsSeal, err := s.receiveEvent(ctx, b, env, p, nil)
	if err != nil {
		return nil, err
	}
	if needsSeal {
		sealed, err := s.seal(ctx, blobBinding{OperationID: p.OperationID, NodeID: b.NodeID, Digest: p.Digest, Kind: env.Type, Seq: p.seq}, env.Body)
		if err != nil {
			return nil, err
		}
		if _, err = s.receiveEvent(ctx, b, env, p, &sealed); err != nil {
			return nil, err
		}
	}
	ack, err := protocol.NewMessage("ack", b.SessionID, env.MessageID, map[string]any{"operation_id": p.OperationID, "event_seq": p.seq})
	if err != nil {
		return nil, ErrStorage
	}
	return ack, nil
}

// receiveEvent executes twice for a new body: first to detect duplicates before
// reserving a wrapping use, then to recheck every predicate and commit the seal.
func (s *Store) receiveEvent(ctx context.Context, b Binding, env protocol.Envelope, p parsedEvent, sealed *sealedBlob) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireBinding(ctx, tx, b, true); err != nil {
		return false, err
	}
	op, err := s.readOperation(ctx, tx, p.OperationID)
	if err != nil {
		return false, err
	}
	if op.NodeID != b.NodeID || op.LedgerID != b.LedgerID {
		return false, ErrIdentity
	}
	if op.Digest != p.Digest {
		return false, ErrConflict
	}
	if err = s.requireOperationEvidence(ctx, tx, op); err != nil {
		return false, err
	}
	if op.ReleaseStatus == "released" {
		if (s.version == 3 || (s.version == 4 || s.version == 5)) && op.LatestResultSeq > 0 {
			return s.receiveReleasedEvent(ctx, tx, op, env, p, sealed)
		}
		return false, errRecoveryReleased
	}
	existing, err := s.readEvent(ctx, tx, op, p.seq)
	if err == nil {
		defer clear(existing.Body)
		if existing.Kind == env.Type && bytes.Equal(existing.Body, env.Body) {
			if err = tx.Commit(); err != nil {
				return false, ErrStorage
			}
			return false, nil
		}
		return false, s.commitEventConflict(ctx, tx, op, p, env.Type, env.Body)
	}
	if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if s.version == 2 || s.version == 3 || (s.version == 4 || s.version == 5) {
		if err = s.validateEventAgainstRecovery(ctx, tx, op, env.Type, p, env.Body); err != nil {
			return false, err
		}
	}
	if s.version == 5 {
		return s.receiveRuntimeEventTx(ctx, tx, b, op, env, p, sealed)
	}
	if op.Blocked {
		return false, ErrFrozen
	}
	// A first result at position one and an execution at position two cannot
	// both be true. A missing started at position one is a permitted replay gap.
	if (p.seq == 1 && env.Type == "result" && op.MaxEventSeq > 0) || (p.seq == 2 && op.LatestResultSeq == 1) {
		return false, s.commitEventConflict(ctx, tx, op, p, env.Type, env.Body)
	}
	// An existing contradictory result must be isolated even when it says no
	// permit was issued. A completely new execution still requires its permit.
	if env.Type == "started" && (op.PermitID == "" || op.PermitID != p.PermitID) {
		return false, ErrConflict
	}
	if env.Type == "result" && p.Outcome != "not_started" && op.PermitID == "" {
		return false, ErrConflict
	}
	if env.Type == "result" && op.LatestResultSeq != 0 {
		return false, ErrEvidence
	}
	if sealed == nil {
		return true, nil
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_events(operation_id,source,event_seq,kind,body_cipher,wrapped_dek,key_id,fingerprint,received_at) VALUES(?,'agent',?,?,?,?,?,?,?)`, op.OperationID, p.seq, env.Type, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint, now); err != nil {
		return false, ErrStorage
	}
	state, release, resultSeq := op.State, op.ReleaseStatus, op.LatestResultSeq
	if env.Type == "result" {
		state, release = resultProjection(p)
		resultSeq = p.seq
	} else if resultSeq == 0 {
		state = "running"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET state=?,release_status=?,latest_result_seq=?,max_event_seq=max(max_event_seq,?),revision=revision+1,updated_at=? WHERE operation_id=?`, state, release, resultSeq, p.seq, now, op.OperationID); err != nil {
		return false, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbox SET delivery_state='confirmed' WHERE operation_id=? AND kind='prepare'`, op.OperationID); err != nil {
		return false, ErrStorage
	}
	if env.Type == "result" {
		if _, err = tx.ExecContext(ctx, `UPDATE outbox SET delivery_state='confirmed' WHERE operation_id=? AND kind='cancel'`, op.OperationID); err != nil {
			return false, ErrStorage
		}
	}
	if env.Type == "result" && release == "pending" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO outbox(operation_id,kind,generation,delivery_state,result_event_seq,terminal_state) VALUES(?,'finalize',1,'pending',?,?)`, op.OperationID, p.seq, state); err != nil {
			return false, ErrStorage
		}
	}
	if state == "reconciliation_required" {
		if _, err = tx.ExecContext(ctx, `UPDATE nodes SET frozen=1 WHERE id=?`, op.NodeID); err != nil {
			return false, ErrStorage
		}
	}
	if (s.version == 4 || s.version == 5) && env.Type == "result" && state == "succeeded" {
		if _, err = tx.ExecContext(ctx, `UPDATE stacks SET last_applied_version_id=? WHERE id=? AND node_id=?`, op.VersionID, op.StackID, op.NodeID); err != nil {
			return false, ErrStorage
		}
	}
	if (s.version == 4 || s.version == 5) && env.Type == "result" && p.Observation != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE stacks SET observation_operation_id=? WHERE id=? AND node_id=?`, op.OperationID, op.StackID, op.NodeID); err != nil {
			return false, ErrStorage
		}
	}
	if err = tx.Commit(); err != nil {
		return false, ErrStorage
	}
	return false, nil
}

func (s *Store) commitEventConflict(ctx context.Context, tx *sql.Tx, op Operation, p parsedEvent, kind string, body []byte) error {
	fingerprint, err := s.fingerprint(body)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO event_conflicts(operation_id,event_seq,kind,fingerprint,created_at) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, op.OperationID, p.seq, kind, fingerprint, now); err != nil {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET blocked=1,state='reconciliation_required',release_status='held',revision=revision+CASE WHEN blocked=0 THEN 1 ELSE 0 END,updated_at=CASE WHEN blocked=0 THEN ? ELSE updated_at END WHERE operation_id=?`, now, op.OperationID); err != nil {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE nodes SET frozen=1 WHERE id=?`, op.NodeID); err != nil {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbox SET delivery_state='suppressed' WHERE operation_id=? AND kind='finalize'`, op.OperationID); err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return ErrConflict
}

func (s *Store) readEvent(ctx context.Context, q querier, op Operation, seq uint64) (Event, error) {
	var e Event
	var sealed sealedBlob
	err := q.QueryRowContext(ctx, `SELECT kind,body_cipher,wrapped_dek,key_id,fingerprint FROM operation_events WHERE operation_id=? AND source='agent' AND event_seq=?`, op.OperationID, seq).Scan(&e.Kind, &sealed.Cipher, &sealed.WrappedDEK, &sealed.KeyID, &sealed.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, ErrStorage
	}
	e.OperationID, e.Digest, e.Seq = op.OperationID, op.Digest, seq
	body, err := s.unseal(blobBinding{OperationID: op.OperationID, NodeID: op.NodeID, Digest: op.Digest, Kind: e.Kind, Seq: seq}, sealed)
	if err != nil {
		return Event{}, err
	}
	defer clear(body)
	if len(body) > maxEventBodyBytes {
		return Event{}, ErrEvidence
	}
	env, err := s.validator.Validate(bodyEnvelope(e.Kind, body))
	if err != nil {
		return Event{}, ErrEvidence
	}
	p, err := parseEvent(env.Body)
	if err != nil || p.OperationID != op.OperationID || p.Digest != op.Digest || p.seq != seq || !s.eventPosition(e.Kind, p) {
		return Event{}, ErrEvidence
	}
	if e.Kind == "started" && (op.PermitID == "" || op.PermitID != p.PermitID) {
		return Event{}, ErrEvidence
	}
	if e.Kind == "result" && p.Outcome != "not_started" && op.PermitID == "" {
		return Event{}, ErrEvidence
	}
	e.Body = append(json.RawMessage(nil), body...)
	return e, nil
}

// ReadEvent returns the exact authenticated body from one transaction snapshot.
func (s *Store) ReadEvent(ctx context.Context, operation string, seq uint64) (Event, error) {
	if !validID(operation) || seq < 1 || seq > s.maxEventSequence() {
		return Event{}, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, ErrStorage
	}
	defer tx.Rollback()
	op, err := s.readOperation(ctx, tx, operation)
	if err != nil {
		return Event{}, err
	}
	if err = s.validateOperationEvents(ctx, tx, op); err != nil {
		return Event{}, err
	}
	return s.readEvent(ctx, tx, op, seq)
}

// validateOperationEvents checks the projection against authenticated evidence
// in the caller's snapshot. A gap before seq2 is legal; an invented result is not.
func (s *Store) validateOperationEvents(ctx context.Context, q querier, op Operation) error {
	if s.version == 5 {
		return s.validateRuntimeEvents(ctx, q, op)
	}
	if op.ReleaseStatus == "released" && op.LatestResultSeq == 0 {
		return s.verifyAbsenceRelease(ctx, q, op)
	}
	if op.ReleaseStatus == "released" {
		if err := s.verifyRelease(ctx, q, op); err != nil {
			return err
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT event_seq FROM operation_events WHERE operation_id=? ORDER BY event_seq`, op.OperationID)
	if err != nil {
		return ErrStorage
	}
	var seqs []uint64
	for rows.Next() {
		var seq uint64
		if err = rows.Scan(&seq); err != nil {
			rows.Close()
			return ErrStorage
		}
		seqs = append(seqs, seq)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return ErrStorage
	}
	var maxSeq, resultSeq uint64
	state, release := "", "held"
	for _, seq := range seqs {
		e, err := s.readEvent(ctx, q, op, seq)
		if err != nil {
			return err
		}
		maxSeq = seq
		if e.Kind == "result" {
			if resultSeq != 0 {
				clear(e.Body)
				return ErrEvidence
			}
			resultSeq = seq
			p, _ := parseEvent(e.Body)
			state, release = resultProjection(p)
		}
		clear(e.Body)
	}
	if maxSeq != op.MaxEventSeq || resultSeq != op.LatestResultSeq || len(seqs) > 2 || (resultSeq == 1 && len(seqs) != 1) {
		return ErrEvidence
	}
	if op.Blocked {
		if op.State != "reconciliation_required" || op.ReleaseStatus != "held" {
			return ErrEvidence
		}
	} else if resultSeq > 0 {
		if state != op.State || (release != op.ReleaseStatus && !((s.version == 3 || (s.version == 4 || s.version == 5)) && op.ReleaseStatus == "released" && release == "pending")) {
			return ErrEvidence
		}
	} else {
		if op.ReleaseStatus != "held" {
			return ErrEvidence
		}
		if maxSeq == 1 {
			if op.State != "running" {
				return ErrEvidence
			}
		} else if op.State != "accepted" && op.State != "ready" && op.State != "authorized" {
			return ErrEvidence
		}
	}
	var intentState, intentTerminal string
	var intentSeq uint64
	err = q.QueryRowContext(ctx, `SELECT delivery_state,result_event_seq,terminal_state FROM outbox WHERE operation_id=? AND kind='finalize' AND generation=1`, op.OperationID).Scan(&intentState, &intentSeq, &intentTerminal)
	if release == "pending" && resultSeq > 0 {
		want := "pending"
		if (s.version == 3 || (s.version == 4 || s.version == 5)) && op.ReleaseStatus == "released" {
			want = "confirmed"
		}
		if op.Blocked {
			want = "suppressed"
		}
		if err != nil || intentState != want || intentSeq != resultSeq || intentTerminal != state {
			return ErrEvidence
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return ErrStorage
		}
		return ErrEvidence
	}
	return nil
}
