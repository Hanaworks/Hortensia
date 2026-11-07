package ledger

import (
	"context"
	_ "embed"
	"sync"
	"time"
)

//go:embed migrations/003_recovery_scan.sql
var schemaV3 string

// RecoveryOptions controls the local monotonic clock. Now must preserve a
// monotonic time base; a nil function uses time.Now.
type RecoveryOptions struct{ Now func() time.Time }

type recoveryConfig struct {
	mu          sync.Mutex
	now         func() time.Time
	localFrozen bool // protected by Store.mu, not this configuration's mutex
	snapshot    *scanSnapshot
	maxRecords  int
	maxBytes    int
}

type ScanState struct {
	Frozen, Dirty                            bool
	SessionID, ActiveScanID, CompletedScanID string
	Total                                    uint64
}

func newRecoveryConfig(options RecoveryOptions) *recoveryConfig {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &recoveryConfig{now: now, localFrozen: true, maxRecords: 4096, maxBytes: 32 << 20}
}

// CreateRecovery creates an explicitly new v3 ledger. It does not upgrade or
// replace an existing ledger and starts frozen until recovery completes.
func CreateRecovery(ctx context.Context, path, nodeID, keyID string, key []byte, journalKey JournalKey, options RecoveryOptions) (*Store, error) {
	return createStore(ctx, path, nodeID, keyID, key, &journalKey, storeFormat{version: 3, schema: schemaV3, recovery: &options})
}

// OpenRecovery preserves the last completed receipt and changes, but freezes
// admission and revokes previous Store writers. Active RAM snapshots expire.
func OpenRecovery(ctx context.Context, path string, expected Identity, key []byte, journalKey JournalKey, options RecoveryOptions) (*Store, error) {
	return openStore(ctx, path, expected, key, &journalKey, storeFormat{version: 3, schema: schemaV3, recovery: &options})
}

func (s *Store) requireRecovery() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return ErrStorage
	}
	if s.recovery == nil {
		return ErrRecovery
	}
	return nil
}

func (s *Store) RecoveryScanState(ctx context.Context) (ScanState, error) {
	if err := s.requireRecovery(); err != nil {
		return ScanState{}, err
	}
	var state ScanState
	if err := s.db.QueryRowContext(ctx, `SELECT frozen,dirty,current_session,active_scan_id,completed_scan_id,active_total FROM recovery_control WHERE id=1`).Scan(&state.Frozen, &state.Dirty, &state.SessionID, &state.ActiveScanID, &state.CompletedScanID, &state.Total); err != nil {
		return ScanState{}, ErrStorage
	}
	return state, nil
}
