package ledger

import (
	"context"
	"database/sql"
	"errors"
)

// PendingProcessOutcomeRefs lists durable exits still missing their first result.
func (s *Store) PendingProcessOutcomeRefs(ctx context.Context, limit int) ([]string, error) {
	if err := s.requireRuntime(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 32 {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoveryWriter(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.operation_id FROM runtime_documents d
		JOIN event_state e ON e.operation_id=d.operation_id
		WHERE d.kind='process' AND d.seq=0 AND e.latest_result_seq IS NULL AND e.blocked=0
		AND NOT EXISTS(SELECT 1 FROM finalizations f WHERE f.operation_id=d.operation_id)
		ORDER BY d.operation_id LIMIT ?`, limit)
	if err != nil {
		return nil, ErrStorage
	}
	refs := []string{}
	for rows.Next() {
		var operation string
		if rows.Scan(&operation) != nil {
			rows.Close()
			return nil, ErrStorage
		}
		refs = append(refs, operation)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil || closeErr != nil {
		return nil, ErrStorage
	}
	for _, operation := range refs {
		r, err := s.readRecord(ctx, tx, operation)
		if err != nil {
			return nil, err
		}
		fact, err := s.readProcessOutcome(ctx, tx, r)
		clear(fact.Output)
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// All exit consumers use the same snapshot-bound admission, including the
// final result transaction. Enumeration alone cannot authorize later writes.
func (s *Store) readProcessOutcome(ctx context.Context, q querier, r Record) (ProcessOutcome, error) {
	var fact ProcessOutcome
	if err := s.readDocument(ctx, q, r, "process", 0, &fact); err != nil {
		return ProcessOutcome{}, err
	}
	if err := validateProcessOutcome(fact); err != nil {
		return ProcessOutcome{}, err
	}
	var boot, spawn string
	if err := q.QueryRowContext(ctx, `SELECT owner_boot,spawn_state FROM process_facts WHERE operation_id=?`, r.OperationID).Scan(&boot, &spawn); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProcessOutcome{}, ErrEvidence
		}
		return ProcessOutcome{}, ErrStorage
	}
	if r.Phase != PhaseStartCommitted || boot != r.BootID || fact.BootID != r.BootID || spawn != "exited" {
		return ProcessOutcome{}, ErrEvidence
	}
	state, err := readEventState(ctx, q, r.OperationID)
	if err != nil {
		return ProcessOutcome{}, err
	}
	if err = s.requireStartedEvent(ctx, q, r); err != nil {
		return ProcessOutcome{}, err
	}
	if !state.LatestResultSeq.Valid {
		if state.LastSeq != 1 {
			return ProcessOutcome{}, ErrEvidence
		}
		if err = requireEventClaims(ctx, q, r); err != nil {
			return ProcessOutcome{}, err
		}
	}
	return fact, nil
}
