package ledger

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/startgate"
)

const gateOtherID = "00000000-0000-4000-8000-000000000099"

func gateReady(t *testing.T, s *Store) (Record, startgate.Request, startgate.Permit) {
	t.Helper()
	r, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	q := startgate.Request{Session: r.PreparedSession, Operation: r.OperationID, Digest: r.Digest,
		Nonce: "00000000-0000-4000-8000-000000000080", MessageID: "00000000-0000-4000-8000-000000000081"}
	p := startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce,
		PermitID: "00000000-0000-4000-8000-000000000082", ReplyTo: q.MessageID, AuthRev: 7}
	return r, q, p
}

func gateAssertFacts(t *testing.T, s *Store, operation, phase string, permit *startgate.Permit) {
	t.Helper()
	r, err := s.Get(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != phase || !reflect.DeepEqual(r.Permit, permit) {
		t.Fatalf("facts: phase=%s permit=%+v, want %s %+v", r.Phase, r.Permit, phase, permit)
	}
	if permit != nil {
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", r.StartedAt); err != nil {
			t.Fatal(err)
		}
	}
	claims, err := s.Claims(context.Background(), operation)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims: %+v %v", claims, err)
	}
}

func TestLedgerGateCommitPrecedesStartAndReplayCannotStartAgain(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, q, p := gateReady(t, s)
	var starts int
	g, err := s.NewGate(context.Background(), q, time.Now, func(got startgate.Permit) {
		starts++
		if got != p {
			t.Errorf("Start permit: %+v", got)
		}
		gateAssertFacts(t, s, p.Operation, PhaseStartCommitted, &p)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrGateExists) {
		t.Fatalf("second gate: %v", err)
	}
	q.MessageID = gateOtherID
	if err := g.RegisterRequest(q); err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(p); err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(p); err != nil {
		t.Fatal(err)
	}
	replayed := p
	replayed.ReplyTo = q.MessageID
	if err := g.Authorize(replayed); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("Start count %d", starts)
	}
	if err := s.commitStart(context.Background(), replayed); !errors.Is(err, ErrAlreadyCommitted) {
		t.Fatalf("durable replay became fresh permission: %v", err)
	}
	for _, change := range []func(*startgate.Permit){func(p *startgate.Permit) { p.PermitID = gateOtherID }, func(p *startgate.Permit) { p.AuthRev++ }} {
		conflict := p
		change(&conflict)
		if err := g.Authorize(conflict); !errors.Is(err, startgate.ErrPermitConflict) {
			t.Fatalf("gate conflict: %v", err)
		}
		if err := s.commitStart(context.Background(), conflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("durable conflict: %v", err)
		}
	}
	gateAssertFacts(t, s, p.Operation, PhaseStartCommitted, &p)
}

func TestLedgerGateRejectsWrongRequestContextAndInvalidIDs(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, q, _ := gateReady(t, s)
	for name, tc := range map[string]struct {
		change func(*startgate.Request)
		want   error
	}{
		"session":              {func(q *startgate.Request) { q.Session = gateOtherID }, ErrConflict},
		"digest":               {func(q *startgate.Request) { q.Digest = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE" }, ErrConflict},
		"missing operation":    {func(q *startgate.Request) { q.Operation = gateOtherID }, ErrNotFound},
		"nonce not UUID":       {func(q *startgate.Request) { q.Nonce = "nonce" }, ErrInput},
		"message not UUID":     {func(q *startgate.Request) { q.MessageID = "message" }, ErrInput},
		"session not UUID":     {func(q *startgate.Request) { q.Session = "session" }, ErrInput},
		"operation not UUID":   {func(q *startgate.Request) { q.Operation = "operation" }, ErrInput},
		"digest not canonical": {func(q *startgate.Request) { q.Digest = "bad" }, ErrInput},
		"UUID uppercase":       {func(q *startgate.Request) { q.Nonce = "ABCDEF00-0000-4000-8000-000000000080" }, ErrInput},
	} {
		t.Run(name, func(t *testing.T) {
			bad := q
			tc.change(&bad)
			if _, err := s.NewGate(context.Background(), bad, time.Now, func(startgate.Permit) {}); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	if _, err := s.NewGate(context.Background(), q, nil, func(startgate.Permit) {}); !errors.Is(err, startgate.ErrRequest) {
		t.Fatalf("nil clock: %v", err)
	}
	if _, err := s.NewGate(context.Background(), q, time.Now, nil); !errors.Is(err, startgate.ErrRequest) {
		t.Fatalf("nil Start: %v", err)
	}
	if _, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerGateRejectsInvalidPermitsBeforePersistence(t *testing.T) {
	for name, change := range map[string]func(*startgate.Permit){
		"session": func(p *startgate.Permit) { p.Session = "bad" }, "operation": func(p *startgate.Permit) { p.Operation = "bad" },
		"nonce": func(p *startgate.Permit) { p.Nonce = "bad" }, "permit": func(p *startgate.Permit) { p.PermitID = "bad" },
		"reply": func(p *startgate.Permit) { p.ReplyTo = "bad" }, "digest": func(p *startgate.Permit) { p.Digest = "bad" },
		"revision": func(p *startgate.Permit) { p.AuthRev = 9007199254740992 },
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := newTestStore(t)
			_, q, p := gateReady(t, s)
			change(&p)
			if err := s.commitStart(context.Background(), p); !errors.Is(err, ErrInput) {
				t.Fatalf("invalid permit: %v", err)
			}
			gateAssertFacts(t, s, q.Operation, PhaseReady, nil)
		})
	}
	// The in-memory gate only checks that IDs are nonempty. Its storage hook must enforce the schema types.
	s, _, _ := newTestStore(t)
	_, q, p := gateReady(t, s)
	starts := 0
	g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) { starts++ })
	if err != nil {
		t.Fatal(err)
	}
	q.MessageID = "invalid-retransmission-id"
	if err := g.RegisterRequest(q); err != nil {
		t.Fatal(err)
	}
	p.ReplyTo = q.MessageID
	if err := g.Authorize(p); !errors.Is(err, ErrInput) {
		t.Fatalf("invalid retransmission reply committed: %v", err)
	}
	if starts != 0 || g.State() != startgate.Uncertain {
		t.Fatalf("starts=%d state=%s", starts, g.State())
	}
	gateAssertFacts(t, s, p.Operation, PhaseReady, nil)
}

func TestLedgerGateRejectsFrozenReadyAndCommitted(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "committed"}[committed], func(t *testing.T) {
			s, path, key := newTestStore(t)
			_, q, p := gateReady(t, s)
			if committed {
				if err := s.commitStart(context.Background(), p); err != nil {
					t.Fatal(err)
				}
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
			if _, err := reopened.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrFrozen) {
				t.Fatalf("old boot gate: %v", err)
			}
			if err := reopened.commitStart(context.Background(), p); !errors.Is(err, ErrFrozen) {
				t.Fatalf("old boot commit: %v", err)
			}
			phase := PhaseReady
			var want *startgate.Permit
			if committed {
				phase = PhaseStartCommitted
				want = &p
			}
			gateAssertFacts(t, reopened, p.Operation, phase, want)
		})
	}
}

func TestLedgerGateRechecksClaimsAndPreparedContextInTransaction(t *testing.T) {
	for name, sql := range map[string]string{
		"missing claim": "DELETE FROM local_claims WHERE resource_kind='project'",
		"wrong claim":   "UPDATE local_claims SET resource_id='wrong' WHERE resource_kind='project'",
		"version":       "UPDATE executions SET version_id='00000000-0000-4000-8000-000000000099'",
		"fingerprint":   "UPDATE executions SET local_fingerprint=zeroblob(32)",
		"session":       "UPDATE executions SET prepared_session='00000000-0000-4000-8000-000000000099'",
		"digest":        "UPDATE executions SET input_digest=x'0101010101010101010101010101010101010101010101010101010101010101'",
		"boot":          "UPDATE executions SET owner_boot_id='00000000-0000-4000-8000-000000000099'",
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := newTestStore(t)
			_, q, p := gateReady(t, s)
			starts := 0
			g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) { starts++ })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			if err := g.Authorize(p); err == nil {
				t.Fatal("changed facts authorized")
			}
			if starts != 0 {
				t.Fatal("started changed facts")
			}
			r, err := s.Get(context.Background(), p.Operation)
			if err != nil || r.Phase != PhaseReady || r.Permit != nil {
				t.Fatalf("modified start facts: %+v %v", r, err)
			}
			if _, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrGateExists) {
				t.Fatalf("failed gate reconstructed: %v", err)
			}
		})
	}
}

func TestLedgerGateCancelOrInvalidateBeforeCommitKeepsReady(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalidate", true: "cancel"}[cancel], func(t *testing.T) {
			s, _, _ := newTestStore(t)
			_, q, p := gateReady(t, s)
			starts := 0
			g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) { starts++ })
			if err != nil {
				t.Fatal(err)
			}
			if cancel {
				if err := g.Cancel(); err != nil {
					t.Fatal(err)
				}
			} else {
				g.Invalidate()
			}
			if err := g.Authorize(p); err == nil {
				t.Fatal("terminal gate authorized")
			}
			if starts != 0 {
				t.Fatal("terminal gate started")
			}
			gateAssertFacts(t, s, p.Operation, PhaseReady, nil)
			if _, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrGateExists) {
				t.Fatalf("terminal gate reconstructed: %v", err)
			}
		})
	}
}

// This test-only adapter controls scheduling around the real SQLite commit.
// It keeps the same registration that Close uses; it never replaces database behavior.
func gateWithBarrier(t *testing.T, s *Store, q startgate.Request, now func() time.Time, commit func(startgate.Permit) error, start func(startgate.Permit)) *startgate.Gate {
	t.Helper()
	g, err := startgate.New(q, startgate.Hooks{Now: now, CommitStart: commit, Start: start})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.gates[q.Operation] = g
	s.mu.Unlock()
	return g
}

func gateAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("barrier timed out")
	}
}
func gateResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(8 * time.Second):
		t.Fatal("operation timed out")
		return nil
	}
}

func TestLedgerGateCommitThenCancelInvalidateOrExpirePreservesFact(t *testing.T) {
	for _, action := range []string{"cancel", "invalidate", "expire"} {
		for _, beforeCommit := range []bool{false, true} {
			label := action + "/after commit"
			if beforeCommit {
				label = action + "/commit callback entered"
			}
			t.Run(label, func(t *testing.T) {
				s, _, _ := newTestStore(t)
				_, q, p := gateReady(t, s)
				entered, release := make(chan struct{}), make(chan struct{})
				var elapsed atomic.Int64
				starts := 0
				base := time.Now()
				g := gateWithBarrier(t, s, q, func() time.Time { return base.Add(time.Duration(elapsed.Load())) }, func(p startgate.Permit) error {
					if beforeCommit {
						close(entered)
						<-release
					}
					err := s.commitStart(context.Background(), p)
					if !beforeCommit {
						close(entered)
						<-release
					}
					return err
				}, func(startgate.Permit) { starts++ })
				done := make(chan error, 1)
				go func() { done <- g.Authorize(p) }()
				gateAwait(t, entered)
				want := startgate.ErrCanceled
				switch action {
				case "cancel":
					if err := g.Cancel(); err != nil {
						t.Fatal(err)
					}
				case "invalidate":
					g.Invalidate()
					want = startgate.ErrInvalidated
				case "expire":
					elapsed.Store(int64(10 * time.Second))
					want = startgate.ErrExpired
				}
				close(release)
				if err := gateResult(t, done); !errors.Is(err, want) {
					t.Fatalf("got %v want %v", err, want)
				}
				if starts != 0 {
					t.Fatal("started after stop/deadline")
				}
				gateAssertFacts(t, s, p.Operation, PhaseStartCommitted, &p)
				if _, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrGateExists) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestLedgerGateExistingCommitAndConcurrentConstructionCannotGrantSecondGate(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, q, p := gateReady(t, s)
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {})
			results <- err
		}()
	}
	created := 0
	for i := 0; i < 8; i++ {
		err := gateResult(t, results)
		if err == nil {
			created++
		} else if !errors.Is(err, ErrGateExists) {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("constructed %d gates", created)
	}
	other, _, _ := newTestStore(t)
	_, request, permit := gateReady(t, other)
	if err := other.commitStart(context.Background(), permit); err != nil {
		t.Fatal(err)
	}
	if _, err := other.NewGate(context.Background(), request, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrAlreadyCommitted) {
		t.Fatalf("existing start granted gate: %v", err)
	}
	gateAssertFacts(t, s, p.Operation, PhaseReady, nil)
}

func TestLedgerGateRejectsMismatchedPermitCorrelationWithoutConsumingGate(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, q, p := gateReady(t, s)
	starts := 0
	g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) { starts++ })
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		change func(*startgate.Permit)
		want   error
	}{
		"session":   {func(p *startgate.Permit) { p.Session = gateOtherID }, startgate.ErrSession},
		"operation": {func(p *startgate.Permit) { p.Operation = gateOtherID }, startgate.ErrOperation},
		"nonce":     {func(p *startgate.Permit) { p.Nonce = gateOtherID }, startgate.ErrNonce},
		"reply":     {func(p *startgate.Permit) { p.ReplyTo = gateOtherID }, startgate.ErrReplyTo},
		"digest":    {func(p *startgate.Permit) { p.Digest = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE" }, startgate.ErrDigest},
	} {
		t.Run(name, func(t *testing.T) {
			bad := p
			tc.change(&bad)
			if err := g.Authorize(bad); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			gateAssertFacts(t, s, p.Operation, PhaseReady, nil)
		})
	}
	if err := g.Authorize(p); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("Start count %d", starts)
	}
}

func TestLedgerGateCloseDuringCommitSuppressesStart(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, q, p := gateReady(t, s)
	entered, release := make(chan struct{}), make(chan struct{})
	starts := 0
	g := gateWithBarrier(t, s, q, time.Now, func(p startgate.Permit) error {
		err := s.commitStart(context.Background(), p)
		close(entered)
		<-release
		return err
	}, func(startgate.Permit) { starts++ })
	done := make(chan error, 1)
	go func() { done <- g.Authorize(p) }()
	gateAwait(t, entered)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := gateResult(t, done); !errors.Is(err, startgate.ErrInvalidated) {
		t.Fatal(err)
	}
	if starts != 0 {
		t.Fatal("closed store started")
	}
	if _, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
}

func TestLedgerGateConcurrentCloseWaitsForStartWithoutBlockingGet(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, q, p := gateReady(t, s)
	entered, readNow, readDone, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	readErr := make(chan error, 1)
	starts := 0
	g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {
		starts++
		close(entered)
		<-readNow
		_, err := s.Get(context.Background(), p.Operation)
		readErr <- err
		close(readDone)
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	authorized := make(chan error, 1)
	go func() { authorized <- g.Authorize(p) }()
	gateAwait(t, entered)
	closed := make(chan error, 2)
	go func() { closed <- s.Close() }()
	go func() { closed <- s.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not start")
		}
		time.Sleep(time.Millisecond)
	}
	close(readNow)
	gateAwait(t, readDone)
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before Start completed: %v", err)
	default:
	}
	close(release)
	if err := gateResult(t, authorized); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := gateResult(t, closed); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Authorize(p); !errors.Is(err, startgate.ErrInvalidated) {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("Start count=%d", starts)
	}
}
