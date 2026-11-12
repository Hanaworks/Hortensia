package ledger

import (
	"context"
	"database/sql"

	"docker-manager/internal/startgate"
)

// Recovery checks are no-ops for v1/v2. They run inside each existing write
// transaction so opening a new v3 Store cannot race a stale writer's decision.
func (s *Store) requireRecoveryWriter(ctx context.Context, q querier) error {
	if s.recovery == nil {
		return nil
	}
	if err := s.requireRecovery(); err != nil {
		return err
	}
	var boot string
	if err := q.QueryRowContext(ctx, `SELECT active_boot FROM recovery_control WHERE id=1`).Scan(&boot); err != nil {
		return ErrStorage
	}
	if boot != s.boot {
		return ErrFrozen
	}
	return nil
}

func (s *Store) requireRecoverySession(ctx context.Context, q querier, session string) error {
	return s.checkRecoveryAccess(ctx, q, session, false)
}

func (s *Store) requireRecoveryMutation(ctx context.Context, q querier, session string) error {
	return s.checkRecoveryAccess(ctx, q, session, true)
}

func (s *Store) checkRecoveryAccess(ctx context.Context, q querier, session string, mutation bool) error {
	if s.recovery == nil {
		return nil
	}
	if err := s.requireRecovery(); err != nil {
		return err
	}
	if mutation {
		if err := s.requireReleaseHealthy(ctx, q); err != nil {
			return err
		}
		// A failed durable freeze must not reopen this process's admission.
		// Never keep this mutex while waiting for the single SQL connection.
		s.mu.Lock()
		frozen := s.recovery.localFrozen
		s.mu.Unlock()
		if frozen {
			return ErrFrozen
		}
	}
	var boot, current string
	var frozen bool
	if err := q.QueryRowContext(ctx, `SELECT active_boot,current_session,frozen FROM recovery_control WHERE id=1`).Scan(&boot, &current, &frozen); err != nil {
		return ErrStorage
	}
	if boot != s.boot || current == "" || current != session || (mutation && frozen) {
		return ErrFrozen
	}
	return nil
}

func (s *Store) setRecoveryLocalFrozen(frozen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovery != nil {
		s.recovery.localFrozen = frozen || s.quarantined
	}
}

// freezeRecoveryGates must be called without Store.mu or a database transaction
// held. Invalidate can wait for a Start callback that reads from this Store.
func (s *Store) freezeRecoveryGates() {
	s.mu.Lock()
	if s.recovery == nil {
		s.mu.Unlock()
		return
	}
	s.recovery.localFrozen = true
	gates := make([]*startgate.Gate, 0, len(s.gates))
	for _, gate := range s.gates {
		gates = append(gates, gate)
	}
	s.mu.Unlock()
	for _, gate := range gates {
		gate.Invalidate()
	}
}

// BindRecoverySessionFixture trusts its caller to have authenticated the new
// session. It cannot prove a real network connection's identity or lifetime.
func (s *Store) BindRecoverySessionFixture(ctx context.Context, session string) error {
	if s.runtime != nil {
		return ErrSchema
	}
	if err := s.requireRecovery(); err != nil {
		return err
	}
	if !validID(session) {
		return ErrInput
	}
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	if err := s.requireRecoveryWriter(ctx, tx); err != nil {
		tx.Rollback()
		return err
	}
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT current_session FROM recovery_control WHERE id=1`).Scan(&current); err != nil {
		tx.Rollback()
		return ErrStorage
	}
	if err := tx.Rollback(); err != nil {
		return ErrStorage
	}
	if current == session {
		return nil
	}
	s.freezeRecoveryGates()
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err := s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recovery_control SET current_session=?,frozen=1 WHERE id=1`, session); err != nil {
		return ErrStorage
	}
	if err := tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) noteRecoveryEvent(ctx context.Context, tx *sql.Tx, operation string, seq uint64) error {
	if s.recovery == nil {
		return nil
	}
	if err := s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_changes(operation_id,event_seq) SELECT ?,? FROM recovery_control WHERE id=1 AND frozen=1 ON CONFLICT DO NOTHING`, operation, seq); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) noteRecoveryConflict(ctx context.Context, tx *sql.Tx) error {
	if s.recovery == nil {
		return nil
	}
	if err := s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recovery_control SET dirty=1 WHERE id=1 AND frozen=1`); err != nil {
		return ErrStorage
	}
	return nil
}
