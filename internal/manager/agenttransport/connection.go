package agenttransport

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"docker-manager/internal/agentwire"
	"docker-manager/internal/manager/operations"
	"docker-manager/internal/protocol"
)

type outbound struct {
	raw      []byte
	delivery *operations.Delivery
}
type requestMeta struct {
	kind, operation, query string
	delivery               bool
}
type retryState struct {
	next    time.Time
	attempt uint
}
type connection struct {
	h             *Handler
	auth          *operations.AuthenticatedNode
	b             operations.Binding
	p             *agentwire.Peer
	ctx           context.Context
	cancel        context.CancelFunc
	once          sync.Once
	workers       sync.WaitGroup
	queue         chan outbound
	notify        chan struct{}
	mu            sync.Mutex
	configuration bool
	sent          map[string]requestMeta
	controls      map[string]retryState
}

func newConnection(h *Handler, parent context.Context, a *operations.AuthenticatedNode, b operations.Binding, p *agentwire.Peer) *connection {
	ctx, cancel := context.WithCancel(parent)
	return &connection{h: h, auth: a, b: b, p: p, ctx: ctx, cancel: cancel, queue: make(chan outbound, 32), notify: make(chan struct{}, 1), sent: map[string]requestMeta{}, controls: map[string]retryState{}}
}
func (c *connection) stop() { c.once.Do(func() { c.cancel(); c.p.Close() }) }
func (c *connection) wake() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}
func (c *connection) enqueue(raw []byte, d *operations.Delivery) error {
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	if d != nil && d.Kind == "prepare" {
		c.mu.Lock()
		if c.configuration {
			c.mu.Unlock()
			return operations.ErrRecoveryBusy
		}
		c.configuration = true
		c.mu.Unlock()
	}
	select {
	case c.queue <- outbound{raw, d}:
		return nil
	default:
		if d != nil && d.Kind == "prepare" {
			c.mu.Lock()
			c.configuration = false
			c.mu.Unlock()
		}
		if d != nil {
			_ = c.h.store.RecordDeliveryFailure(c.ctx, c.b, d.MessageID, "queue_full", time.Now())
		}
		return agentwire.ErrBudget
	}
}

func (c *connection) run() {
	c.workers.Add(2)
	go func() { defer c.workers.Done(); c.writeLoop() }()
	go func() { defer c.workers.Done(); c.driveLoop() }()
	defer func() { c.stop(); c.workers.Wait() }()
	for {
		raw, e, err := c.p.Read(c.ctx)
		if err != nil {
			return
		}
		if c.h.store.RecordNodeSeen(c.ctx, c.auth, c.b.SessionID) != nil {
			return
		}
		if c.receive(raw, e) != nil {
			return
		}
		c.wake()
	}
}

func (c *connection) recordSent(raw []byte, d *operations.Delivery) error {
	var e protocol.Envelope
	if json.Unmarshal(raw, &e) != nil {
		return agentwire.ErrProtocol
	}
	switch e.Type {
	case "observe", "reconcile", "reconcile_complete", "prepare", "finalize", "cancel":
	default:
		return nil
	}
	var body struct {
		Operation string `json:"operation_id"`
		Query     string `json:"query_id"`
	}
	if json.Unmarshal(e.Body, &body) != nil {
		return agentwire.ErrProtocol
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, known := c.sent[e.MessageID]; !known && len(c.sent) >= 65536 {
		return agentwire.ErrBudget
	}
	c.sent[e.MessageID] = requestMeta{e.Type, body.Operation, body.Query, d != nil}
	return nil
}

func (c *connection) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.queue:
			if err := c.recordSent(msg.raw, msg.delivery); err != nil {
				c.stop()
				return
			}
			err := c.p.Write(c.ctx, msg.raw)
			if msg.delivery != nil && msg.delivery.Kind == "prepare" {
				c.mu.Lock()
				c.configuration = false
				c.mu.Unlock()
			}
			if err != nil {
				if msg.delivery != nil {
					_ = c.h.store.RecordDeliveryFailure(c.ctx, c.b, msg.delivery.MessageID, "write_failed", time.Now())
				}
				c.stop()
				return
			}
		}
	}
}

func (c *connection) receive(raw []byte, e protocol.Envelope) error {
	s := c.h.store
	switch e.Type {
	case "ready":
		return s.ReceiveReady(c.ctx, c.auth, c.b.SessionID, raw)
	case "request_start":
		answer, err := s.AuthorizeStart(c.ctx, c.b, raw)
		if err != nil {
			if !errors.Is(err, operations.ErrPermission) && !errors.Is(err, operations.ErrConflict) && !errors.Is(err, operations.ErrFrozen) {
				return err
			}
			var body struct {
				Operation string `json:"operation_id"`
				Nonce     string `json:"start_nonce"`
			}
			json.Unmarshal(e.Body, &body)
			code := "recovery_required"
			if errors.Is(err, operations.ErrPermission) {
				code = "permission_denied"
			}
			answer, err = protocol.NewMessage("deny_start", c.b.SessionID, e.MessageID, map[string]any{"operation_id": body.Operation, "start_nonce": body.Nonce, "code": code})
		}
		if err != nil {
			return err
		}
		return c.enqueue(answer, nil)
	case "started", "result":
		answer, err := s.ReceiveEvent(c.ctx, c.b, raw)
		if err != nil {
			return err
		}
		return c.enqueue(answer, nil)
	case "finalized":
		return s.ReceiveFinalized(c.ctx, c.b, raw)
	case "reconcile_reply":
		_, err := s.ReceiveRecoveryPage(c.ctx, c.b, raw)
		return err
	case "reconcile_completed":
		_, err := s.ReceiveRecoveryCompleted(c.ctx, c.b, raw)
		return err
	case "observation":
		return c.observation(e)
	case "protocol_error":
		return agentwire.ErrProtocol
	default:
		return agentwire.ErrProtocol
	}
}

func (c *connection) observation(e protocol.Envelope) error {
	if e.ReplyTo == nil {
		return agentwire.ErrProtocol
	}
	c.mu.Lock()
	sent, ok := c.sent[*e.ReplyTo]
	c.mu.Unlock()
	if !ok || sent.kind != "observe" {
		return agentwire.ErrProtocol
	}
	var body struct {
		Query     string  `json:"query_id"`
		Operation *string `json:"operation_id"`
		Node      string  `json:"node_id"`
		Error     *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &body) != nil || body.Query != sent.query || body.Node != c.b.NodeID || body.Operation == nil || *body.Operation != sent.operation {
		return agentwire.ErrProtocol
	}
	if body.Error != nil {
		code := "observation_failed"
		if body.Error.Code == "cursor_expired" {
			code = "cursor_expired"
		}
		if sent.delivery {
			if err := c.h.store.RecordDeliveryFailure(c.ctx, c.b, *e.ReplyTo, code, time.Now()); err != nil {
				return err
			}
		}
		return agentwire.ErrTransport
	}
	return nil
}

func expectedWait(err error) bool {
	return errors.Is(err, operations.ErrRecoveryIncomplete) || errors.Is(err, operations.ErrRecoveryBusy)
}
func (c *connection) queueControl(raw []byte) error {
	var e protocol.Envelope
	if json.Unmarshal(raw, &e) != nil {
		return agentwire.ErrProtocol
	}
	now := time.Now()
	r, known := c.controls[e.MessageID]
	if !known && len(c.controls) >= 65536 {
		return agentwire.ErrBudget
	}
	if now.Before(r.next) {
		return nil
	}
	if err := c.enqueue(raw, nil); err != nil {
		return err
	}
	r.attempt++
	shift := r.attempt - 1
	if shift > 5 {
		shift = 5
	}
	delay := time.Second * time.Duration(uint64(1)<<shift)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	r.next = now.Add(delay)
	c.controls[e.MessageID] = r
	return nil
}

func (c *connection) driveLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	c.wake()
	var lastPong time.Time
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
		case <-c.notify:
		}
		if pong := c.p.LastPongAt(); pong.After(lastPong) {
			if c.h.store.RecordNodeSeen(c.ctx, c.auth, c.b.SessionID) != nil {
				c.stop()
				return
			}
			lastPong = pong
		}
		if err := c.drive(); err != nil && !expectedWait(err) {
			c.stop()
			return
		}
	}
}

func (c *connection) drive() error {
	s := c.h.store
	view, err := s.ReadRuntimeConnectionState(c.ctx, c.b)
	if err != nil {
		return err
	}
	state := view.Recovery
	if state.RoundID == "" || state.Status == "expired" || (state.Status == "completed" && view.Frozen) {
		raw, err := s.StartRecovery(c.ctx, c.b, "pending", 200)
		if err != nil {
			return err
		}
		return c.queueControl(raw)
	}
	switch state.Status {
	case "collecting":
		raw, err := s.NextRecoveryRequest(c.ctx, c.b)
		if err != nil {
			return err
		}
		if err = c.queueControl(raw); err != nil {
			return err
		}
	case "completing":
		raw, err := s.PrepareRecoveryComplete(c.ctx, c.b)
		if err != nil {
			return err
		}
		if err = c.queueControl(raw); err != nil {
			return err
		}
	case "completed":
	default:
		state, err = s.ProcessRecovery(c.ctx, c.b)
		if err != nil {
			return err
		}
		if state.Status == "need_all" {
			raw, err := s.StartRecovery(c.ctx, c.b, "all", 200)
			if err != nil {
				return err
			}
			return c.queueControl(raw)
		}
		if state.Status == "ready" {
			raw, err := s.PrepareRecoveryComplete(c.ctx, c.b)
			if err != nil {
				return err
			}
			if err = c.queueControl(raw); err != nil {
				return err
			}
		}
	}
	c.mu.Lock()
	busy := c.configuration
	c.mu.Unlock()
	if busy {
		return nil
	}
	delivery, err := s.NextNodeDelivery(c.ctx, c.b, time.Now())
	if err != nil {
		return err
	}
	if delivery != nil {
		return c.enqueue(delivery.Raw, delivery)
	}
	return nil
}
