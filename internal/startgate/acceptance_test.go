package startgate

import (
	"errors"
	"testing"
	"time"
)

// A denial arriving at the deadline must not replace an expired request with
// a denied result, nor make a late permit executable.
func TestDenialAtDeadlinePreservesExpiry(t *testing.T) {
	commits, starts := 0, 0
	g, clock := newTestGate(t, func(Permit) error { commits++; return nil }, func(Permit) { starts++ })
	clock.Advance(10 * time.Second)
	if err := g.Deny(denial()); !errors.Is(err, ErrExpired) {
		t.Fatalf("deny error=%v, want expiry", err)
	}
	if err := g.Authorize(permit()); !errors.Is(err, ErrExpired) {
		t.Fatalf("authorize error=%v, want expiry", err)
	}
	if g.State() != Expired || commits != 0 || starts != 0 {
		t.Fatalf("state=%s commits=%d starts=%d", g.State(), commits, starts)
	}
}

// Commit failure must preserve an intervening cancellation/invalidation while
// still reporting the failed commit and never allowing another execution.
func TestFailedCommitPreservesInterveningStop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stop  func(*Gate)
		state State
		err   error
	}{
		{"cancel", func(g *Gate) { _ = g.Cancel() }, Canceled, ErrCanceled},
		{"invalidate", func(g *Gate) { g.Invalidate() }, Invalidated, ErrInvalidated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commits, starts := 0, 0
			commitErr := errors.New("injected commit failure")
			var g *Gate
			g, _ = newTestGate(t, func(Permit) error {
				commits++
				if g.State() != Committing {
					t.Fatalf("commit entered with state=%s", g.State())
				}
				tc.stop(g)
				return commitErr
			}, func(Permit) { starts++ })
			if err := g.Authorize(permit()); !errors.Is(err, ErrCommitFailed) || !errors.Is(err, commitErr) {
				t.Fatalf("commit error=%v", err)
			}
			if err := g.Authorize(permit()); !errors.Is(err, tc.err) {
				t.Fatalf("retry error=%v want=%v", err, tc.err)
			}
			if g.State() != tc.state || commits != 1 || starts != 0 {
				t.Fatalf("state=%s commits=%d starts=%d", g.State(), commits, starts)
			}
		})
	}
}

// Advancing time is not a method call: State does not fabricate an observed
// expiry, and Cancel is still permitted before the next pending check.
func TestCancellationAfterUnobservedDeadlineRemainsCanceled(t *testing.T) {
	commits, starts := 0, 0
	g, clock := newTestGate(t, func(Permit) error { commits++; return nil }, func(Permit) { starts++ })
	clock.Advance(11 * time.Second)
	if g.State() != Waiting {
		t.Fatalf("clock advancement changed state=%s", g.State())
	}
	for i := 0; i < 2; i++ {
		if err := g.Cancel(); err != nil {
			t.Fatalf("cancel=%v", err)
		}
	}
	if err := g.Authorize(permit()); !errors.Is(err, ErrCanceled) {
		t.Fatalf("authorize=%v", err)
	}
	if g.State() != Canceled || commits != 0 || starts != 0 {
		t.Fatalf("state=%s commits=%d starts=%d", g.State(), commits, starts)
	}
}

func TestMaximumSafeAuthorizationRevisionCanStart(t *testing.T) {
	commits, starts := 0, 0
	g, _ := newTestGate(t, func(p Permit) error {
		commits++
		if p.AuthRev != 9007199254740991 {
			t.Fatalf("commit changed revision=%d", p.AuthRev)
		}
		return nil
	}, func(p Permit) {
		starts++
		if p.AuthRev != 9007199254740991 {
			t.Fatalf("start changed revision=%d", p.AuthRev)
		}
	})
	p := permit()
	p.AuthRev = 9007199254740991
	if err := g.Authorize(p); err != nil {
		t.Fatal(err)
	}
	if g.State() != Started || commits != 1 || starts != 1 {
		t.Fatalf("state=%s commits=%d starts=%d", g.State(), commits, starts)
	}
}
