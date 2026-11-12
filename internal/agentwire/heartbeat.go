package agentwire

import (
	"context"
	"errors"
	"github.com/coder/websocket"
	"sync"
	"sync/atomic"
	"time"
)

var errHeartbeatSilence = errors.New("heartbeat_silence")
var errHeartbeatBudget = errors.New("heartbeat_pending_budget")
var errHeartbeatTransport = errors.New("heartbeat_transport_failure")

type heartbeatTicker interface {
	C() <-chan time.Time
	Stop()
}
type heartbeatTimer interface {
	C() <-chan time.Time
	Reset(time.Duration)
	Stop()
}
type heartbeatClock interface {
	Now() time.Time
	NewTicker(time.Duration) heartbeatTicker
	NewTimer(time.Duration) heartbeatTimer
}

// heartbeat separates the periodic send cadence from the last-Pong deadline.
// This probe permits at most four pending Ping calls. Receiving unsolicited or
// mismatched Pongs can keep the silence deadline alive without completing those
// calls; exhaustion is a transport-budget failure, not a 45-second silence claim.
// A heartbeat belongs to one connection and Run must be called only once.
type heartbeat struct {
	clock        heartbeatClock
	ping         func(context.Context, *websocket.Conn) error
	close        func(*websocket.Conn)
	mu           sync.Mutex
	lastPong     time.Time
	receivedPong bool
	pongs        chan struct{}
	inFlight     atomic.Int32
}

func newHeartbeat() *heartbeat {
	return &heartbeat{clock: realHeartbeatClock{}, pongs: make(chan struct{}, 1),
		ping:  func(ctx context.Context, c *websocket.Conn) error { return c.Ping(ctx) },
		close: func(c *websocket.Conn) { c.CloseNow() },
	}
}

// Pong is the bounded, nonblocking OnPongReceived hook. Payload is intentionally
// ignored: any received Pong refreshes liveness, while Conn.Ping correlates its
// own reply. The channel reader must keep running concurrently with Run.
func (h *heartbeat) Pong(context.Context, string) {
	h.mu.Lock()
	h.lastPong = h.clock.Now()
	h.receivedPong = true
	h.mu.Unlock()
	select {
	case h.pongs <- struct{}{}:
	default:
	}
}

func (h *heartbeat) silenceRemaining() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastPong.Add(45 * time.Second).Sub(h.clock.Now())
}

// Run sends at the independent 15-second cadence and closes after 45 seconds
// without a Pong, on a transport failure, or when ctx is canceled. It cancels
// and reaps every pending Ping before returning, closing the connection to
// unblock underlying I/O. The caller owns the concurrent message reader.
func (h *heartbeat) Run(ctx context.Context, c *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.lastPong = h.clock.Now()
	h.mu.Unlock()
	ticker := h.clock.NewTicker(15 * time.Second)
	timer := h.clock.NewTimer(45 * time.Second)
	var workers sync.WaitGroup
	results := make(chan error, 4)
	defer func() {
		cancel()
		h.close(c)
		workers.Wait()
		ticker.Stop()
		timer.Stop()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.pongs:
			remaining := h.silenceRemaining()
			if remaining <= 0 {
				return errHeartbeatSilence
			}
			timer.Reset(remaining)
		case <-timer.C():
			remaining := h.silenceRemaining()
			if remaining <= 0 {
				return errHeartbeatSilence
			}
			timer.Reset(remaining)
		case err := <-results:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return errHeartbeatTransport
			}
		case <-ticker.C():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if h.silenceRemaining() <= 0 {
				return errHeartbeatSilence
			}
			// Account for completions already queued before judging the budget.
		drain:
			for {
				select {
				case err := <-results:
					if err != nil {
						return errHeartbeatTransport
					}
				default:
					break drain
				}
			}
			if h.inFlight.Load() >= 4 {
				return errHeartbeatBudget
			}
			h.inFlight.Add(1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer h.inFlight.Add(-1)
				err := h.ping(ctx, c)
				select {
				case results <- err:
				case <-ctx.Done():
				}
			}()
		}
	}
}

type realHeartbeatClock struct{}

func (realHeartbeatClock) Now() time.Time { return time.Now() }
func (realHeartbeatClock) NewTicker(d time.Duration) heartbeatTicker {
	return realHeartbeatTicker{time.NewTicker(d)}
}
func (realHeartbeatClock) NewTimer(d time.Duration) heartbeatTimer {
	return realHeartbeatTimer{time.NewTimer(d)}
}

type realHeartbeatTicker struct{ *time.Ticker }

func (t realHeartbeatTicker) C() <-chan time.Time { return t.Ticker.C }

type realHeartbeatTimer struct{ *time.Timer }

func (t realHeartbeatTimer) C() <-chan time.Time   { return t.Timer.C }
func (t realHeartbeatTimer) Reset(d time.Duration) { t.Timer.Reset(d) }
func (t realHeartbeatTimer) Stop()                 { t.Timer.Stop() }
