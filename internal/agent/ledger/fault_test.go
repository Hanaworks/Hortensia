package ledger

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/startgate"
)

func gateAssertFailureFrozen(t *testing.T, s *Store, g *startgate.Gate, q startgate.Request, p startgate.Permit, starts int) {
	t.Helper()
	if starts != 0 || g.State() != startgate.Uncertain {
		t.Fatalf("starts=%d gate=%s", starts, g.State())
	}
	if err := g.Authorize(p); !errors.Is(err, startgate.ErrUncertain) {
		t.Fatalf("uncertain gate retried: %v", err)
	}
	if _, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrGateExists) {
		t.Fatalf("uncertain gate rebuilt: %v", err)
	}
	gateAssertFacts(t, s, p.Operation, PhaseReady, nil)
}

func TestLedgerGateRealWriterLockTimeoutAndContextCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy timeout", true: "context cancellation"}[cancel], func(t *testing.T) {
			s, path, _ := newTestStore(t)
			_, q, p := gateReady(t, s)
			starts := 0
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) { starts++ })
			if err != nil {
				t.Fatal(err)
			}
			other, err := sql.Open("sqlite", databaseURI(path))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			other.SetMaxOpenConns(1)
			lock, err := other.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback()
			if _, err := lock.Exec("UPDATE agent_meta SET schema_version=schema_version WHERE id=1"); err != nil {
				t.Fatal(err)
			}
			begin := time.Now()
			done := make(chan error, 1)
			go func() { done <- g.Authorize(p) }()
			if cancel {
				time.AfterFunc(100*time.Millisecond, stop)
			}
			err = gateResult(t, done)
			elapsed := time.Since(begin)
			if !errors.Is(err, ErrStorage) || !errors.Is(err, startgate.ErrCommitFailed) {
				t.Fatalf("lock failure: %v", err)
			}
			if cancel {
				if elapsed > 2*time.Second {
					t.Fatalf("context failed to stop busy wait: %s", elapsed)
				}
			} else {
				if elapsed < 4*time.Second || elapsed > 7*time.Second {
					t.Fatalf("5000ms busy timeout elapsed %s", elapsed)
				}
			}
			if err := lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			gateAssertFailureFrozen(t, s, g, q, p, starts)
		})
	}
}

func TestLedgerGateRealStatementAbortAndCommitFailure(t *testing.T) {
	for _, commitFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "statement abort", true: "deferred foreign key commit"}[commitFailure], func(t *testing.T) {
			s, path, key := newTestStore(t)
			_, q, p := gateReady(t, s)
			starts := 0
			g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) { starts++ })
			if err != nil {
				t.Fatal(err)
			}
			if commitFailure {
				// The trigger's statement is legal; only COMMIT checks this deferred FK.
				if _, err := s.db.Exec(`CREATE TABLE fault_commit(parent TEXT REFERENCES executions(operation_id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_start AFTER UPDATE OF phase ON executions BEGIN INSERT INTO fault_commit(parent) VALUES('private-secret-missing-parent'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.db.Exec(`CREATE TRIGGER fail_start BEFORE UPDATE OF phase ON executions BEGIN SELECT RAISE(ABORT,'private-secret-input'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = g.Authorize(p)
			if !errors.Is(err, ErrStorage) || !errors.Is(err, startgate.ErrCommitFailed) {
				t.Fatalf("failure diagnosis: %v", err)
			}
			if strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), "SQLITE") {
				t.Fatalf("driver details escaped: %v", err)
			}
			gateAssertFailureFrozen(t, s, g, q, p, starts)
			if commitFailure {
				var n int
				if err := s.db.QueryRow("SELECT count(*) FROM fault_commit").Scan(&n); err != nil || n != 0 {
					t.Fatalf("failed transaction retained writes: %d %v", n, err)
				}
			}
			if _, err := s.db.Exec("DROP TRIGGER fail_start"); err != nil {
				t.Fatal(err)
			}
			if commitFailure {
				if _, err := s.db.Exec("DROP TABLE fault_commit"); err != nil {
					t.Fatal(err)
				}
			}
			// Removal of the injected fault cannot revive the old gate.
			if err := g.Authorize(p); !errors.Is(err, startgate.ErrUncertain) {
				t.Fatalf("fault removal revived gate: %v", err)
			}
			identity := s.Identity()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(context.Background(), path, identity, key)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			gateAssertFacts(t, reopened, p.Operation, PhaseReady, nil)
			if _, err := reopened.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrFrozen) {
				t.Fatalf("reopen revived failed start: %v", err)
			}
		})
	}
}

func TestLedgerGateConcurrentDurableCASHasOneFirstCommit(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, _, p := gateReady(t, s)
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { done <- s.commitStart(context.Background(), p) }()
	}
	first := 0
	for i := 0; i < 8; i++ {
		err := gateResult(t, done)
		if err == nil {
			first++
		} else if !errors.Is(err, ErrAlreadyCommitted) {
			t.Fatal(err)
		}
	}
	if first != 1 {
		t.Fatalf("first successful commits: %d", first)
	}
	gateAssertFacts(t, s, p.Operation, PhaseStartCommitted, &p)
}
