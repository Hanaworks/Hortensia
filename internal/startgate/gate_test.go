package startgate

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testClock struct {
	mu   sync.Mutex
	time time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.time }
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.time = c.time.Add(d)
}

func request() Request {
	return Request{Session: "session-1", Operation: "operation-1", Digest: "digest-1", Nonce: "nonce-1", MessageID: "request-1"}
}

func permit() Permit {
	return Permit{Session: "session-1", Operation: "operation-1", Digest: "digest-1", Nonce: "nonce-1", PermitID: "permit-1", AuthRev: 3, ReplyTo: "request-1"}
}

func denial() Denial {
	return Denial{Session: "session-1", Operation: "operation-1", Nonce: "nonce-1", ReplyTo: "request-1", Code: "permission_denied"}
}

func newTestGate(t *testing.T, commit func(Permit) error, start func(Permit)) (*Gate, *testClock) {
	t.Helper()
	c := &testClock{time: time.Now()}
	g, err := New(request(), Hooks{Now: c.Now, CommitStart: commit, Start: start})
	if err != nil {
		t.Fatal(err)
	}
	return g, c
}

func awaitCommit(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("CommitStart was not called")
	}
}

func TestValidPermitCommitsBeforeStarting(t *testing.T) {
	var sequence []string
	g, _ := newTestGate(t, func(p Permit) error {
		if p != permit() {
			t.Fatalf("committed wrong permit: %+v", p)
		}
		sequence = append(sequence, "commit")
		return nil
	}, func(p Permit) { sequence = append(sequence, "start") })
	if err := g.Authorize(permit()); err != nil {
		t.Fatal(err)
	}
	if len(sequence) != 2 || sequence[0] != "commit" || sequence[1] != "start" {
		t.Fatalf("sequence = %v", sequence)
	}
	if g.State() != Started {
		t.Fatalf("state = %s", g.State())
	}
}

func TestDuplicatePermitNeverStartsTwice(t *testing.T) {
	commits, starts := 0, 0
	g, clock := newTestGate(t, func(Permit) error { commits++; return nil }, func(Permit) { starts++ })
	for i := 0; i < 3; i++ {
		if err := g.Authorize(permit()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(11 * time.Second)
	}
	if commits != 1 || starts != 1 {
		t.Fatalf("commits=%d starts=%d", commits, starts)
	}
}

func TestPermitCorrelationRejectsWrongFieldsWithoutConsumingRequest(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Permit)
		want   error
	}{
		{"session", func(p *Permit) { p.Session = "old-session" }, ErrSession},
		{"operation", func(p *Permit) { p.Operation = "other-operation" }, ErrOperation},
		{"digest", func(p *Permit) { p.Digest = "other-digest" }, ErrDigest},
		{"nonce", func(p *Permit) { p.Nonce = "old-nonce" }, ErrNonce},
		{"reply_to", func(p *Permit) { p.ReplyTo = "unknown-request" }, ErrReplyTo},
		{"empty_permit", func(p *Permit) { p.PermitID = "" }, ErrPermit},
		{"auth_revision_budget", func(p *Permit) { p.AuthRev = 9007199254740992 }, ErrPermit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commits, starts := 0, 0
			g, _ := newTestGate(t, func(Permit) error { commits++; return nil }, func(Permit) { starts++ })
			bad := permit()
			tt.change(&bad)
			if err := g.Authorize(bad); !errors.Is(err, tt.want) {
				t.Fatalf("error=%v want=%v", err, tt.want)
			}
			if commits != 0 || starts != 0 {
				t.Fatalf("invalid permit caused commits=%d starts=%d", commits, starts)
			}
			if err := g.Authorize(permit()); err != nil {
				t.Fatal(err)
			}
			if starts != 1 {
				t.Fatalf("valid permit did not start: %d", starts)
			}
		})
	}
}

func TestSameNonceKeepsAllRequestCorrelations(t *testing.T) {
	for _, reply := range []string{"request-1", "request-2"} {
		t.Run(reply, func(t *testing.T) {
			starts := 0
			g, _ := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
			retry := request()
			retry.MessageID = "request-2"
			if err := g.RegisterRequest(retry); err != nil {
				t.Fatal(err)
			}
			p := permit()
			p.ReplyTo = reply
			if err := g.Authorize(p); err != nil {
				t.Fatal(err)
			}
			p.ReplyTo = "request-2"
			if err := g.Authorize(p); err != nil {
				t.Fatal(err)
			}
			if starts != 1 {
				t.Fatalf("starts=%d", starts)
			}
		})
	}
}

func TestRequestRetryDoesNotExtendTenSecondWindow(t *testing.T) {
	starts := 0
	g, clock := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
	clock.Advance(9 * time.Second)
	retry := request()
	retry.MessageID = "request-2"
	if err := g.RegisterRequest(retry); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	p := permit()
	p.ReplyTo = "request-2"
	if err := g.Authorize(p); !errors.Is(err, ErrExpired) {
		t.Fatalf("error=%v", err)
	}
	if starts != 0 {
		t.Fatalf("starts=%d", starts)
	}
}

func TestPermitJustBeforeDeadlineStarts(t *testing.T) {
	starts := 0
	g, clock := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
	clock.Advance(10*time.Second - time.Nanosecond)
	if err := g.Authorize(permit()); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("starts=%d", starts)
	}
}

func TestChangedRequestCannotReplaceIdentityOrNonce(t *testing.T) {
	for _, field := range []string{"session", "operation", "digest", "nonce", "empty_message_id"} {
		t.Run(field, func(t *testing.T) {
			starts := 0
			g, _ := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
			r := request()
			switch field {
			case "session":
				r.Session = "new-session"
			case "operation":
				r.Operation = "new-operation"
			case "digest":
				r.Digest = "new-digest"
			case "nonce":
				r.Nonce = "new-nonce"
			case "empty_message_id":
				r.MessageID = ""
			}
			if err := g.RegisterRequest(r); err == nil {
				t.Fatal("changed request accepted")
			}
			if err := g.Authorize(permit()); err != nil {
				t.Fatal(err)
			}
			if starts != 1 {
				t.Fatalf("starts=%d", starts)
			}
		})
	}
}

func TestCancellationPreventsLatePermit(t *testing.T) {
	commits, starts := 0, 0
	g, _ := newTestGate(t, func(Permit) error { commits++; return nil }, func(Permit) { starts++ })
	if err := g.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(permit()); !errors.Is(err, ErrCanceled) {
		t.Fatalf("error=%v", err)
	}
	if commits != 0 || starts != 0 {
		t.Fatalf("commits=%d starts=%d", commits, starts)
	}
}

func TestConnectionReplacementInvalidatesOldPermit(t *testing.T) {
	commits, starts := 0, 0
	g, _ := newTestGate(t, func(Permit) error { commits++; return nil }, func(Permit) { starts++ })
	g.Invalidate()
	if err := g.Authorize(permit()); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("error=%v", err)
	}
	if err := g.RegisterRequest(request()); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("retry error=%v", err)
	}
	if commits != 0 || starts != 0 {
		t.Fatalf("commits=%d starts=%d", commits, starts)
	}
}

func TestCommitFailureNeverStartsOrRetries(t *testing.T) {
	commits, starts := 0, 0
	diskErr := errors.New("commit rejected")
	g, _ := newTestGate(t, func(Permit) error { commits++; return diskErr }, func(Permit) { starts++ })
	if err := g.Authorize(permit()); !errors.Is(err, ErrCommitFailed) || !errors.Is(err, diskErr) {
		t.Fatalf("error=%v", err)
	}
	if err := g.Authorize(permit()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("retry error=%v", err)
	}
	if commits != 1 || starts != 0 {
		t.Fatalf("commits=%d starts=%d", commits, starts)
	}
}

func TestExpiryDuringCommitPreventsStart(t *testing.T) {
	starts := 0
	var clock *testClock
	g, c := newTestGate(t, func(Permit) error { clock.Advance(10 * time.Second); return nil }, func(Permit) { starts++ })
	clock = c
	if err := g.Authorize(permit()); !errors.Is(err, ErrExpired) {
		t.Fatalf("error=%v", err)
	}
	if starts != 0 {
		t.Fatalf("starts=%d", starts)
	}
}

func TestInvalidationAndCancellationDoNotWaitForCommit(t *testing.T) {
	for _, action := range []string{"invalidate", "cancel"} {
		t.Run(action, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var starts atomic.Int32
			g, _ := newTestGate(t, func(Permit) error { close(entered); <-release; return nil }, func(Permit) { starts.Add(1) })
			completed := make(chan error, 1)
			go func() { completed <- g.Authorize(permit()) }()
			awaitCommit(t, entered)
			actionDone := make(chan error, 1)
			go func() {
				if action == "invalidate" {
					g.Invalidate()
					actionDone <- nil
				} else {
					actionDone <- g.Cancel()
				}
			}()
			select {
			case err := <-actionDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				close(release)
				<-completed
				t.Fatal("invalidation blocked behind CommitStart")
			}
			close(release)
			want := ErrInvalidated
			if action == "cancel" {
				want = ErrCanceled
			}
			if err := <-completed; !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
			if starts.Load() != 0 {
				t.Fatalf("starts=%d", starts.Load())
			}
		})
	}
}

func TestConcurrentDuplicateCannotEnterCommitTwice(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var commits, starts atomic.Int32
	g, _ := newTestGate(t, func(Permit) error { commits.Add(1); close(entered); <-release; return nil }, func(Permit) { starts.Add(1) })
	done := make(chan error, 1)
	go func() { done <- g.Authorize(permit()) }()
	awaitCommit(t, entered)
	if err := g.Authorize(permit()); !errors.Is(err, ErrCommitting) {
		t.Fatalf("error=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if commits.Load() != 1 || starts.Load() != 1 {
		t.Fatalf("commits=%d starts=%d", commits.Load(), starts.Load())
	}
}

func TestChangedPermitIdentityIsNotAnIdempotentReplay(t *testing.T) {
	for _, field := range []string{"permit_id", "auth_rev"} {
		t.Run(field, func(t *testing.T) {
			starts := 0
			g, _ := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
			if err := g.Authorize(permit()); err != nil {
				t.Fatal(err)
			}
			p := permit()
			if field == "permit_id" {
				p.PermitID = "another-permit"
			} else {
				p.AuthRev++
			}
			if err := g.Authorize(p); !errors.Is(err, ErrPermitConflict) {
				t.Fatalf("error=%v", err)
			}
			if starts != 1 {
				t.Fatalf("starts=%d", starts)
			}
		})
	}
}

func TestDenyBeforeAuthorizationPreventsStart(t *testing.T) {
	starts := 0
	g, _ := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
	if err := g.Deny(denial()); err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(permit()); !errors.Is(err, ErrDenied) {
		t.Fatalf("error=%v", err)
	}
	if starts != 0 || g.State() != Denied {
		t.Fatalf("starts=%d state=%s", starts, g.State())
	}
}

func TestLateDenyAndCancelCannotOverwriteStartedFact(t *testing.T) {
	starts := 0
	g, _ := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
	if err := g.Authorize(permit()); err != nil {
		t.Fatal(err)
	}
	if err := g.Deny(denial()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("deny error=%v", err)
	}
	if err := g.Cancel(); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("cancel error=%v", err)
	}
	g.Invalidate()
	if starts != 1 || g.State() != Started {
		t.Fatalf("starts=%d state=%s", starts, g.State())
	}
}

func TestUnrelatedDenialCannotCancelPendingRequest(t *testing.T) {
	for _, field := range []string{"session", "operation", "nonce", "reply_to", "empty_code"} {
		t.Run(field, func(t *testing.T) {
			starts := 0
			g, _ := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
			d := denial()
			switch field {
			case "session":
				d.Session = "other-session"
			case "operation":
				d.Operation = "other-operation"
			case "nonce":
				d.Nonce = "other-nonce"
			case "reply_to":
				d.ReplyTo = "unknown-request"
			case "empty_code":
				d.Code = ""
			}
			if err := g.Deny(d); err == nil {
				t.Fatal("unrelated denial accepted")
			}
			if err := g.Authorize(permit()); err != nil {
				t.Fatal(err)
			}
			if starts != 1 {
				t.Fatalf("starts=%d", starts)
			}
		})
	}
}

func TestIncompleteContextCannotCreateAnExecutableGate(t *testing.T) {
	for _, field := range []string{"session", "operation", "digest", "nonce", "message_id", "clock", "commit", "start"} {
		t.Run(field, func(t *testing.T) {
			r := request()
			h := Hooks{Now: time.Now, CommitStart: func(Permit) error { return nil }, Start: func(Permit) {}}
			switch field {
			case "session":
				r.Session = ""
			case "operation":
				r.Operation = ""
			case "digest":
				r.Digest = ""
			case "nonce":
				r.Nonce = ""
			case "message_id":
				r.MessageID = ""
			case "clock":
				h.Now = nil
			case "commit":
				h.CommitStart = nil
			case "start":
				h.Start = nil
			}
			if _, err := New(r, h); err == nil {
				t.Fatal("incomplete context accepted")
			}
		})
	}
}

func TestDisconnectedGateRejectsReplayButKeepsStartedFact(t *testing.T) {
	starts := 0
	g, _ := newTestGate(t, func(Permit) error { return nil }, func(Permit) { starts++ })
	if err := g.Authorize(permit()); err != nil {
		t.Fatal(err)
	}
	g.Invalidate()
	if err := g.Authorize(permit()); !errors.Is(err, ErrInvalidated) {
		t.Fatalf("replay error=%v", err)
	}
	if starts != 1 || g.State() != Started {
		t.Fatalf("starts=%d state=%s", starts, g.State())
	}
}

func TestDenialDuringCommitCannotOverwriteCommitment(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	g, _ := newTestGate(t, func(Permit) error { close(entered); <-release; return nil }, func(Permit) { starts.Add(1) })
	done := make(chan error, 1)
	go func() { done <- g.Authorize(permit()) }()
	awaitCommit(t, entered)
	err := g.Deny(denial())
	close(release)
	if !errors.Is(err, ErrCommitting) {
		t.Fatalf("denial error=%v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || g.State() != Started {
		t.Fatalf("starts=%d state=%s", starts.Load(), g.State())
	}
}
