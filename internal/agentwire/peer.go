// Package agentwire implements the production v1 transport boundary. Business
// authentication, correlation, durable recovery and execution remain in Stores.
package agentwire

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
	"unicode/utf8"

	"docker-manager/internal/protocol"
	"github.com/coder/websocket"
)

const maxMessage = 8 << 20
const handshakeTimeout = 10 * time.Second
const maxSeenIDs = 65536

var (
	ErrUnauthorized = errors.New("agent authentication rejected")
	ErrProtocol     = errors.New("agent wire protocol rejected")
	ErrTransport    = errors.New("agent wire connection unavailable")
	ErrBudget       = errors.New("agent wire identity budget exhausted")
	ErrConfig       = errors.New("agent wire configuration invalid")
)

type ClientConfig struct {
	URL, Token string
	RootCAs    *x509.CertPool
	Hello      []byte
}
type Side uint8

const (
	Manager Side = iota
	Agent
)

type Peer struct {
	conn      *websocket.Conn
	v         *protocol.Validator
	side      Side
	session   string
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	writes    chan struct{}
	seen      map[string][32]byte
	seenLimit int
	liveness  *heartbeat
}

func newPeer(parent context.Context, c *websocket.Conn, v *protocol.Validator, side Side, session string, h *heartbeat, initial protocol.Envelope) *Peer {
	ctx, cancel := context.WithCancel(parent)
	p := &Peer{conn: c, v: v, side: side, session: session, ctx: ctx, cancel: cancel, done: make(chan struct{}), writes: make(chan struct{}, 1), seen: map[string][32]byte{}, seenLimit: maxSeenIDs, liveness: h}
	p.seen[initial.MessageID] = envelopeFingerprint(initial)
	p.writes <- struct{}{}
	go func() { defer close(p.done); defer cancel(); _ = h.Run(ctx, c) }()
	return p
}

func envelopeFingerprint(e protocol.Envelope) [32]byte {
	fixed, _ := json.Marshal([]any{e.Type, e.SessionID, e.ReplyTo, string(e.Body)})
	return sha256.Sum256(fixed)
}

func (p *Peer) SessionID() string     { return p.session }
func (p *Peer) Done() <-chan struct{} { return p.done }

// LastPongAt reports only a received control frame, not the initial liveness
// deadline. A Store adapter can touch last_seen without inventing traffic.
func (p *Peer) LastPongAt() time.Time {
	p.liveness.mu.Lock()
	defer p.liveness.mu.Unlock()
	if !p.liveness.receivedPong {
		return time.Time{}
	}
	return p.liveness.lastPong
}
func (p *Peer) stop() { p.closeOnce.Do(func() { p.cancel(); _ = p.conn.CloseNow() }) }
func (p *Peer) Close() {
	if p == nil {
		return
	}
	p.stop()
	<-p.done
}

func direction(side Side, kind string, inbound bool) bool {
	fromAgent := side == Manager
	if !inbound {
		fromAgent = !fromAgent
	}
	if kind == "protocol_error" {
		return true
	}
	if fromAgent {
		switch kind {
		case "ready", "request_start", "started", "result", "finalized", "reconcile_reply", "reconcile_completed", "observation":
			return true
		}
	} else {
		switch kind {
		case "prepare", "authorize_start", "deny_start", "ack", "cancel", "observe", "finalize", "reconcile", "reconcile_complete":
			return true
		}
	}
	return false
}

func (p *Peer) validate(raw []byte, inbound bool) (protocol.Envelope, error) {
	e, err := p.v.Validate(raw)
	if err != nil || e.SessionID == nil || *e.SessionID != p.session || !direction(p.side, e.Type, inbound) {
		return protocol.Envelope{}, ErrProtocol
	}
	return e, nil
}

func (p *Peer) Read(ctx context.Context) ([]byte, protocol.Envelope, error) {
	typ, raw, err := p.conn.Read(ctx)
	if err != nil {
		p.stop()
		if ctx.Err() != nil {
			return nil, protocol.Envelope{}, ctx.Err()
		}
		return nil, protocol.Envelope{}, ErrTransport
	}
	if typ != websocket.MessageText || !utf8.Valid(raw) {
		p.stop()
		return nil, protocol.Envelope{}, ErrProtocol
	}
	e, err := p.validate(raw, true)
	if err != nil {
		p.stop()
		return nil, protocol.Envelope{}, err
	}
	// Fingerprint the immutable semantic envelope and raw body, not transport
	// whitespace outside body. A repeated ID cannot change type/correlation.
	fingerprint := envelopeFingerprint(e)
	if old, exists := p.seen[e.MessageID]; exists {
		if old != fingerprint {
			p.stop()
			return nil, protocol.Envelope{}, ErrProtocol
		}
	} else {
		if len(p.seen) >= p.seenLimit {
			p.stop()
			return nil, protocol.Envelope{}, ErrBudget
		}
		p.seen[e.MessageID] = fingerprint
	}
	return raw, e, nil
}

func (p *Peer) Write(ctx context.Context, raw []byte) error {
	if _, err := p.validate(raw, false); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrTransport
	case <-p.writes:
	}
	defer func() { p.writes <- struct{}{} }()
	write, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.conn.Write(write, websocket.MessageText, raw); err != nil {
		p.stop()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrTransport
	}
	return nil
}

type Handshake struct {
	conn        *websocket.Conn
	v           *protocol.Validator
	parent      context.Context
	deadline    time.Time
	liveness    *heartbeat
	hello       protocol.Envelope
	transferred bool
}

func Accept(w http.ResponseWriter, r *http.Request) (*Handshake, error) {
	if r.TLS == nil || r.Method != "GET" || r.URL.Path != "/agent/v1/connect" || r.URL.RawQuery != "" || r.URL.ForceQuery || len(r.Header.Values("Origin")) != 0 || len(r.Header.Values("Cookie")) != 0 {
		http.Error(w, "Connection rejected.", 403)
		return nil, ErrProtocol
	}
	v, err := protocol.NewValidator()
	if err != nil {
		return nil, ErrConfig
	}
	h := newHeartbeat()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled, OnPongReceived: func(ctx context.Context, _ []byte) { h.Pong(ctx, "") }})
	if err != nil {
		return nil, ErrTransport
	}
	c.SetReadLimit(maxMessage)
	return &Handshake{conn: c, v: v, parent: r.Context(), deadline: time.Now().Add(handshakeTimeout), liveness: h}, nil
}

func (h *Handshake) Close() {
	if h != nil && !h.transferred {
		_ = h.conn.CloseNow()
	}
}
func (h *Handshake) ReadHello(ctx context.Context) ([]byte, error) {
	if h.transferred || h.hello.MessageID != "" {
		return nil, ErrProtocol
	}
	ctx, cancel := context.WithDeadline(ctx, h.deadline)
	defer cancel()
	typ, raw, err := h.conn.Read(ctx)
	if err != nil || typ != websocket.MessageText || !utf8.Valid(raw) {
		h.Close()
		return nil, ErrProtocol
	}
	e, err := h.v.Validate(raw)
	if err != nil || e.Type != "hello" {
		h.Close()
		return nil, ErrProtocol
	}
	h.hello = e
	return raw, nil
}

func welcomeValid(v *protocol.Validator, raw []byte, hello protocol.Envelope) (protocol.Envelope, error) {
	e, err := v.Validate(raw)
	if err != nil || e.Type != "welcome" || e.SessionID == nil || e.ReplyTo == nil || *e.ReplyTo != hello.MessageID {
		return protocol.Envelope{}, ErrProtocol
	}
	var original struct {
		NodeID string `json:"node_id"`
	}
	var body struct {
		NodeID            string `json:"node_id"`
		SessionID         string `json:"session_id"`
		ReconcileRequired bool   `json:"reconcile_required"`
	}
	if json.Unmarshal(hello.Body, &original) != nil || json.Unmarshal(e.Body, &body) != nil || original.NodeID != body.NodeID || body.SessionID != *e.SessionID || !body.ReconcileRequired {
		return protocol.Envelope{}, ErrProtocol
	}
	return e, nil
}

func (h *Handshake) Welcome(ctx context.Context, raw []byte) (*Peer, error) {
	if h.transferred || h.hello.MessageID == "" {
		return nil, ErrProtocol
	}
	e, err := welcomeValid(h.v, raw, h.hello)
	if err != nil {
		h.Close()
		return nil, err
	}
	ctx, cancel := context.WithDeadline(ctx, h.deadline)
	defer cancel()
	if err := h.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		h.Close()
		return nil, ErrTransport
	}
	h.transferred = true
	return newPeer(h.parent, h.conn, h.v, Manager, *e.SessionID, h.liveness, h.hello), nil
}

func Dial(parent context.Context, cfg ClientConfig) (*Peer, []byte, error) {
	u, err := url.Parse(cfg.URL)
	secret, secretErr := base64.RawURLEncoding.Strict().DecodeString(cfg.Token)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.Path != "/agent/v1/connect" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || secretErr != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != cfg.Token {
		return nil, nil, ErrConfig
	}
	clear(secret)
	v, err := protocol.NewValidator()
	if err != nil {
		return nil, nil, ErrConfig
	}
	hello, err := v.Validate(cfg.Hello)
	if err != nil || hello.Type != "hello" {
		return nil, nil, ErrConfig
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(parent, handshakeTimeout)
	defer cancel()
	h := newHeartbeat()
	c, response, err := websocket.Dial(ctx, cfg.URL, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.Token}}, CompressionMode: websocket.CompressionDisabled, OnPongReceived: func(ctx context.Context, _ []byte) { h.Pong(ctx, "") }})
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if response != nil && response.StatusCode == 401 {
			return nil, nil, ErrUnauthorized
		}
		return nil, nil, ErrTransport
	}
	c.SetReadLimit(maxMessage)
	fail := func(err error) (*Peer, []byte, error) { _ = c.CloseNow(); return nil, nil, err }
	if err := c.Write(ctx, websocket.MessageText, cfg.Hello); err != nil {
		return fail(ErrTransport)
	}
	typ, raw, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText || !utf8.Valid(raw) {
		return fail(ErrProtocol)
	}
	welcome, err := welcomeValid(v, raw, hello)
	if err != nil {
		return fail(err)
	}
	return newPeer(parent, c, v, Agent, *welcome.SessionID, h, welcome), raw, nil
}
