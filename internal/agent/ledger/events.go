package ledger

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
)

const maxEventBodyBytes = 7 << 20

// Event contains the exact original body. AckSession is the last trusted
// connection acknowledgement; it never authorizes releasing local claims.
type Event struct {
	OperationID, Digest, Kind string
	Seq                       uint64
	Body                      json.RawMessage
	AckSession                string
}

type eventState struct {
	LastSeq         uint64
	LatestResultSeq sql.NullInt64
	Blocked         bool
}

func readEventState(ctx context.Context, q querier, operation string) (eventState, error) {
	var state eventState
	if err := q.QueryRowContext(ctx, `SELECT last_seq,latest_result_seq,blocked FROM event_state WHERE operation_id=?`, operation).Scan(&state.LastSeq, &state.LatestResultSeq, &state.Blocked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return eventState{}, ErrEvidence
		}
		return eventState{}, ErrStorage
	}
	if state.LastSeq > 2 || (state.LatestResultSeq.Valid && (state.LatestResultSeq.Int64 < 1 || uint64(state.LatestResultSeq.Int64) != state.LastSeq)) {
		return eventState{}, ErrEvidence
	}
	return state, nil
}

func requireEventClaims(ctx context.Context, q querier, r Record) error {
	claims, err := readClaims(ctx, q, r.OperationID)
	if err != nil {
		return err
	}
	if len(claims) != 2 || claims[0] != (Claim{"project", projectResourceID(r.EndpointID, r.ProjectName)}) || claims[1] != (Claim{"stack", r.StackID}) {
		return ErrEvidence
	}
	return nil
}

// eventBodyBinding is parsed only after Validator has accepted the entire
// message, including mathematical integers such as 1.0 and 1e0.
func eventBodyBinding(body []byte, kind string) (eventBinding, error) {
	var fields struct {
		Operation string      `json:"operation_id"`
		Digest    string      `json:"input_digest"`
		Seq       json.Number `json:"event_seq"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return eventBinding{}, ErrInput
	}
	seq, err := fields.Seq.Float64()
	if err != nil || seq < 1 || seq > 9007199254740991 || seq != float64(uint64(seq)) {
		return eventBinding{}, ErrInput
	}
	return eventBinding{OperationID: fields.Operation, Digest: fields.Digest, Kind: kind, Seq: uint64(seq)}, nil
}

func (s *Store) readEvent(ctx context.Context, q querier, operation string, seq uint64) (Event, error) {
	var event Event
	var sealed sealedEvent
	var digest []byte
	var ack sql.NullString
	err := q.QueryRowContext(ctx, `SELECT e.kind,e.body_cipher,e.wrapped_dek,e.key_id,e.body_fingerprint,e.ack_session,x.input_digest FROM execution_events e JOIN executions x ON x.operation_id=e.operation_id WHERE e.operation_id=? AND e.event_seq=?`, operation, seq).Scan(&event.Kind, &sealed.Cipher, &sealed.WrappedDEK, &sealed.KeyID, &sealed.Fingerprint, &ack, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, ErrStorage
	}
	event.OperationID, event.Seq, event.Digest, event.AckSession = operation, seq, base64.RawURLEncoding.EncodeToString(digest), ack.String
	binding := eventBinding{OperationID: operation, Digest: event.Digest, Kind: event.Kind, Seq: seq}
	body, err := s.openEvent(binding, sealed)
	if err != nil {
		return Event{}, err
	}
	if len(body) > maxEventBodyBytes || (event.Kind != "started" && event.Kind != "result") || (ack.Valid && !validID(ack.String)) {
		clear(body)
		return Event{}, ErrEvidence
	}
	// Encode only the header, then append the original body. Re-encoding the
	// body could expand HTML or Unicode escapes past the wire budget even when
	// the original accepted message fits. This envelope is validation-only.
	raw, err := json.Marshal(map[string]any{"protocol": 1, "type": event.Kind, "message_id": s.identity.NodeID, "session_id": s.identity.NodeID, "reply_to": nil})
	if err != nil {
		clear(body)
		return Event{}, ErrEvidence
	}
	raw = append(raw[:len(raw)-1], []byte(`,"body":`)...)
	raw = append(raw, body...)
	raw = append(raw, '}')
	if _, err = s.validator.Validate(raw); err != nil {
		clear(body)
		return Event{}, ErrEvidence
	}
	decoded, err := eventBodyBinding(body, event.Kind)
	if err != nil || decoded != binding {
		clear(body)
		return Event{}, ErrEvidence
	}
	event.Body = body
	return event, nil
}

func (s *Store) ReadEvent(ctx context.Context, operationID string, seq uint64) (Event, error) {
	if err := s.requireJournal(); err != nil {
		return Event{}, err
	}
	if !validID(operationID) || seq < 1 || seq > 9007199254740991 {
		return Event{}, ErrInput
	}
	return s.readEvent(ctx, s.db, operationID, seq)
}

func (s *Store) PendingEvents(ctx context.Context, operationID, sessionID string) ([]Event, error) {
	if err := s.requireJournal(); err != nil {
		return nil, err
	}
	if !validID(operationID) || !validID(sessionID) {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if _, err := s.readRecord(ctx, tx, operationID); err != nil {
		return nil, err
	}
	// Materialize and close the cursor before decrypting events on this one
	// connection. Every returned body/ack belongs to this same transaction.
	rows, err := tx.QueryContext(ctx, `SELECT event_seq FROM execution_events WHERE operation_id=? AND (ack_session IS NULL OR ack_session<>?) ORDER BY event_seq`, operationID, sessionID)
	if err != nil {
		return nil, ErrStorage
	}
	seqs := make([]uint64, 0, 2)
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return nil, ErrStorage
		}
		seqs = append(seqs, seq)
		if len(seqs) > 2 {
			rows.Close()
			return nil, ErrEvidence
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, ErrStorage
	}
	if err := rows.Close(); err != nil {
		return nil, ErrStorage
	}
	events := make([]Event, 0, len(seqs))
	for _, seq := range seqs {
		event, err := s.readEvent(ctx, tx, operationID, seq)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	return events, nil
}

// AcknowledgeEvent is a trusted storage call. Its caller must first verify the
// authenticated connection, message direction, current session and reply_to.
func (s *Store) AcknowledgeEvent(ctx context.Context, operationID, digest string, seq uint64, sessionID string) error {
	if err := s.requireJournal(); err != nil {
		return err
	}
	if !validID(operationID) || !validID(sessionID) || !validDigest(digest) || seq < 1 || seq > 9007199254740991 {
		return ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	event, err := s.readEvent(ctx, tx, operationID, seq)
	if err != nil {
		return err
	}
	if event.Digest != digest {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_events SET ack_session=? WHERE operation_id=? AND event_seq=?`, sessionID, operationID, seq); err != nil {
		return ErrStorage
	}
	if err := tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}
