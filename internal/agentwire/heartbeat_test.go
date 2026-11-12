package agentwire

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakeHeartbeatClock struct {
	mu      sync.Mutex
	now     time.Time
	ticks   chan time.Time
	timer   *fakeHeartbeatTimer
	ready   chan struct{}
	stopped atomic.Int32
	period  time.Duration
}
type fakeHeartbeatTimer struct {
	fired   chan time.Time
	resets  chan time.Duration
	stopped atomic.Int32
}
type fakeHeartbeatTicker struct{ clock *fakeHeartbeatClock }

func (c *fakeHeartbeatClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeHeartbeatClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}
func (c *fakeHeartbeatClock) NewTicker(d time.Duration) heartbeatTicker {
	c.period = d
	return &fakeHeartbeatTicker{c}
}
func (c *fakeHeartbeatClock) NewTimer(d time.Duration) heartbeatTimer {
	c.timer.resets <- d
	close(c.ready)
	return c.timer
}
func (t *fakeHeartbeatTicker) C() <-chan time.Time  { return t.clock.ticks }
func (t *fakeHeartbeatTicker) Stop()                { t.clock.stopped.Add(1) }
func (t *fakeHeartbeatTimer) C() <-chan time.Time   { return t.fired }
func (t *fakeHeartbeatTimer) Reset(d time.Duration) { t.resets <- d }
func (t *fakeHeartbeatTimer) Stop()                 { t.stopped.Add(1) }

type heartbeatFixture struct {
	h      *heartbeat
	clock  *fakeHeartbeatClock
	calls  chan chan error
	active atomic.Int32
	closed atomic.Int32
	cancel context.CancelFunc
	done   chan error
}

func startHeartbeatTest(t *testing.T) *heartbeatFixture {
	t.Helper()
	f := &heartbeatFixture{h: newHeartbeat(), calls: make(chan chan error, 8), done: make(chan error, 1)}
	f.clock = &fakeHeartbeatClock{now: time.Now(), ticks: make(chan time.Time, 8), ready: make(chan struct{}), timer: &fakeHeartbeatTimer{fired: make(chan time.Time, 8), resets: make(chan time.Duration, 16)}}
	f.h.clock = f.clock
	f.h.ping = func(ctx context.Context, _ *websocket.Conn) error {
		f.active.Add(1)
		defer f.active.Add(-1)
		reply := make(chan error)
		f.calls <- reply
		select {
		case err := <-reply:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.h.close = func(*websocket.Conn) { f.closed.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() { f.done <- f.h.Run(ctx, nil) }()
	t.Cleanup(cancel)
	select {
	case <-f.clock.ready:
	case err := <-f.done:
		t.Fatalf("heartbeat returned before running: %v", err)
	case <-time.After(time.Second):
		t.Fatal("heartbeat failed to initialize")
	}
	if d := <-f.clock.timer.resets; d != 45*time.Second {
		t.Fatalf("initial silence budget=%v", d)
	}
	if f.clock.period != 15*time.Second {
		t.Fatalf("ping cadence=%v", f.clock.period)
	}
	return f
}

func (f *heartbeatFixture) tick(t *testing.T, d time.Duration) chan error {
	t.Helper()
	f.clock.advance(d)
	f.clock.ticks <- f.clock.Now()
	select {
	case call := <-f.calls:
		return call
	case err := <-f.done:
		t.Fatalf("heartbeat stopped: %v", err)
	case <-time.After(time.Second):
		t.Fatal("scheduled ping did not start")
	}
	return nil
}

func (f *heartbeatFixture) pong(t *testing.T) {
	t.Helper()
	f.h.Pong(context.Background(), "test-pong")
	select {
	case d := <-f.clock.timer.resets:
		if d != 45*time.Second {
			t.Fatalf("Pong did not reset full silence window: %v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("Pong did not refresh deadline")
	}
}

func (f *heartbeatFixture) finish(t *testing.T, want error) {
	t.Helper()
	select {
	case err := <-f.done:
		if !errors.Is(err, want) {
			t.Fatalf("error=%v want=%v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not stop")
	}
	if f.active.Load() != 0 || f.closed.Load() != 1 || f.clock.stopped.Load() != 1 || f.clock.timer.stopped.Load() != 1 {
		t.Fatalf("cleanup active=%d closed=%d ticker=%d timer=%d", f.active.Load(), f.closed.Load(), f.clock.stopped.Load(), f.clock.timer.stopped.Load())
	}
}

func TestHeartbeatSendsWhileEarlierPingWaits(t *testing.T) {
	f := startHeartbeatTest(t)
	f.tick(t, 15*time.Second)
	f.tick(t, 15*time.Second)
	if f.active.Load() != 2 {
		t.Fatalf("pending pings=%d", f.active.Load())
	}
	f.cancel()
	f.finish(t, context.Canceled)
}

func TestHeartbeatHealthyIdleConnectionStaysOpen(t *testing.T) {
	f := startHeartbeatTest(t)
	for i := 0; i < 8; i++ {
		reply := f.tick(t, 15*time.Second)
		f.pong(t)
		reply <- nil
		// Let this completed virtual network round trip finish before advancing
		// the virtual clock by another fifteen seconds.
		until := time.Now().Add(time.Second)
		for f.h.inFlight.Load() != 0 {
			if time.Now().After(until) {
				t.Fatal("completed Ping was not reaped")
			}
			runtime.Gosched()
		}
	}
	f.cancel()
	f.finish(t, context.Canceled)
}

func TestHeartbeatLatePongRefreshesDeadline(t *testing.T) {
	f := startHeartbeatTest(t)
	first := f.tick(t, 15*time.Second)
	f.clock.advance(14 * time.Second)
	f.pong(t)
	first <- nil           // Last Pong at t=29.
	f.tick(t, time.Second) // New Ping at t=30.
	f.clock.advance(30 * time.Second)
	f.clock.timer.fired <- f.clock.Now() // t=60 is only 31 seconds silent.
	select {
	case d := <-f.clock.timer.resets:
		if d != 14*time.Second {
			t.Fatalf("remaining silence budget=%v", d)
		}
	case err := <-f.done:
		t.Fatalf("closed before 45s silence: %v", err)
	case <-time.After(time.Second):
		t.Fatal("silence timer was not rescheduled")
	}
	f.clock.advance(14 * time.Second)
	f.clock.timer.fired <- f.clock.Now()
	f.finish(t, errHeartbeatSilence)
}

func TestHeartbeatMissingPongsCloseAtSilenceDeadline(t *testing.T) {
	f := startHeartbeatTest(t)
	f.tick(t, 15*time.Second)
	f.tick(t, 15*time.Second)
	f.clock.advance(15 * time.Second)
	f.clock.timer.fired <- f.clock.Now()
	f.finish(t, errHeartbeatSilence)
}

func TestHeartbeatCancellationReapsPendingPings(t *testing.T) {
	f := startHeartbeatTest(t)
	f.tick(t, 15*time.Second)
	f.tick(t, 15*time.Second)
	f.cancel()
	f.finish(t, context.Canceled)
}

func TestHeartbeatInFlightBudgetIsDistinctFromSilence(t *testing.T) {
	f := startHeartbeatTest(t)
	for i := 0; i < 4; i++ {
		f.tick(t, 15*time.Second)
		f.pong(t)
	}
	f.clock.advance(15 * time.Second)
	f.clock.ticks <- f.clock.Now()
	f.finish(t, errHeartbeatBudget)
}

func TestHeartbeatTransportFailureCleansUp(t *testing.T) {
	f := startHeartbeatTest(t)
	reply := f.tick(t, 15*time.Second)
	reply <- errors.New("injected transport failure")
	f.finish(t, errHeartbeatTransport)
}
