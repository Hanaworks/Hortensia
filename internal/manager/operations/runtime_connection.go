package operations

import (
	"context"
	"errors"
)

// RuntimeConnectionState is a consistent view for the transport coordinator.
// It grants no authority; all subsequent operations revalidate the Binding.
type RuntimeConnectionState struct {
	Frozen   bool
	Recovery RecoveryState
}

func (s *Store) ReadRuntimeConnectionState(ctx context.Context, b Binding) (RuntimeConnectionState, error) {
	var state RuntimeConnectionState
	if s.version != 5 {
		return state, ErrSchema
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state, s.storageFailure(err)
	}
	defer tx.Rollback()
	if err := s.requireBinding(ctx, tx, b, true); err != nil {
		return state, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT frozen FROM nodes WHERE id=?`, b.NodeID).Scan(&state.Frozen); err != nil {
		return state, s.storageFailure(err)
	}
	scan, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return state, err
	}
	if err == nil {
		state.Recovery = scan.RecoveryState
	}
	if err := tx.Commit(); err != nil {
		return state, s.storageFailure(err)
	}
	return state, nil
}
