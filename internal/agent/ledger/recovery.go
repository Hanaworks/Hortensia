package ledger

import (
	"context"
	"encoding/json"
	"errors"
)

// RecoveryRecord projects one operation from a single database snapshot. It is
// not a scan, a result inference, or permission to resume execution or release
// claims. The projection uses the existing agent-v1 RecoveryRecord fields.
func (s *Store) RecoveryRecord(ctx context.Context, operationID string) (json.RawMessage, error) {
	if err := s.requireJournal(); err != nil {
		return nil, err
	}
	if !validID(operationID) {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	return s.readRecoveryRecord(ctx, tx, operationID)
}

// readRecoveryRecord uses its caller's snapshot and never opens another
// connection. Recovery scans need every record from this same transaction.
func (s *Store) readRecoveryRecord(ctx context.Context, q querier, operationID string) (json.RawMessage, error) {
	record, err := s.readRecord(ctx, q, operationID)
	if err != nil {
		return nil, err
	}
	state, err := readEventState(ctx, q, operationID)
	if err != nil {
		return nil, err
	}
	_, final, finalErr := s.readFinalization(ctx, q, record, state)
	if errors.Is(finalErr, ErrNotFound) {
		if err := requireEventClaims(ctx, q, record); err != nil {
			return nil, err
		}
	} else if finalErr != nil {
		return nil, finalErr
	}
	// This slice has only the initial started event and one result. A pointer
	// without its complete ordered history is not a coherent recovery record.
	if state.LastSeq > 2 {
		return nil, ErrEvidence
	}
	var count uint64
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM execution_events WHERE operation_id=?", operationID).Scan(&count); err != nil {
		return nil, ErrStorage
	}
	if count != state.LastSeq {
		return nil, ErrEvidence
	}
	var latest *Event
	for seq := uint64(1); seq <= state.LastSeq; seq++ {
		event, err := s.readEvent(ctx, q, operationID, seq)
		if err != nil {
			return nil, err
		}
		if event.Digest != record.Digest {
			return nil, ErrEvidence
		}
		if record.Phase == PhaseStartCommitted && seq == 1 {
			if event.Kind != "started" || record.Permit == nil {
				return nil, ErrEvidence
			}
			var started struct {
				PermitID  string `json:"permit_id"`
				StartedAt string `json:"started_at"`
			}
			if err := json.Unmarshal(event.Body, &started); err != nil || started.PermitID != record.Permit.PermitID || started.StartedAt != record.StartedAt {
				return nil, ErrEvidence
			}
		} else {
			if event.Kind != "result" || latest != nil {
				return nil, ErrEvidence
			}
			copy := event
			latest = &copy
		}
	}
	if record.Phase == PhaseStartCommitted && state.LastSeq == 0 {
		return nil, ErrEvidence
	}
	if record.Phase == PhaseReady && state.LastSeq > 1 {
		return nil, ErrEvidence
	}
	if (latest != nil) != state.LatestResultSeq.Valid {
		return nil, ErrEvidence
	}
	if latest != nil && uint64(state.LatestResultSeq.Int64) != latest.Seq {
		return nil, ErrEvidence
	}
	phase := record.Phase
	var resultSeq any
	var result json.RawMessage
	acked := false
	if latest != nil {
		phase, resultSeq, result, acked = "result", latest.Seq, latest.Body, latest.AckSession != ""
	}
	if state.Blocked || (latest == nil && record.Phase == PhaseStartCommitted && record.BootID != s.boot) {
		phase = "reconciliation_required"
	}
	view := struct {
		OperationID string          `json:"operation_id"`
		Digest      string          `json:"input_digest"`
		StackID     string          `json:"stack_id"`
		VersionID   string          `json:"version_id"`
		ProjectName string          `json:"project_name"`
		Phase       string          `json:"phase"`
		LastSeq     uint64          `json:"last_event_seq"`
		ResultSeq   any             `json:"result_event_seq"`
		Latest      json.RawMessage `json:"-"`
		ClaimsHeld  bool            `json:"claims_held"`
		ReportAcked bool            `json:"report_acked"`
		FinalState  any             `json:"final_state"`
		FinalSeq    any             `json:"finalized_result_event_seq"`
	}{record.OperationID, record.Digest, record.StackID, record.VersionID, record.ProjectName, phase, state.LastSeq, resultSeq, result, true, acked, nil, nil}
	if finalErr == nil {
		view.Phase, view.ClaimsHeld, view.FinalState, view.FinalSeq = "finalized", false, final.Terminal, final.Seq
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, ErrEvidence
	}
	// The authenticated body has already passed the wire validator. Append it
	// unchanged: marshaling RawMessage would compact and HTML-escape its bytes,
	// potentially turning an accepted 7 MiB body into an oversized message.
	if result == nil {
		result = json.RawMessage("null")
	}
	raw = append(raw[:len(raw)-1], []byte(`,"latest_result":`)...)
	raw = append(raw, result...)
	raw = append(raw, '}')
	return raw, nil
}
