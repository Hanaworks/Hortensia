// Package channel is an M0 transport probe, not a deployable Agent.
// Every connection remains frozen. It owns no operation or recovery ledger.
package channel

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"docker-manager/internal/protocol"
	"github.com/coder/websocket"
)

const maxMessage = 8 * 1024 * 1024
const handshakeTimeout = 10 * time.Second

var ErrUnauthorized = errors.New("node authentication rejected; automatic retry must stop")
var errProtocol = errors.New("protocol violation")
var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Config struct{ NodeID, LedgerID, Token string }
type ClientConfig struct {
	URL, NodeID, LedgerID, Token, ServerName string
	RootCAs                                  *x509.CertPool
}
type Server struct {
	cfg         Config
	v           *protocol.Validator
	mu          sync.Mutex
	current     *Peer
	closed      bool
	connections map[*websocket.Conn]struct{}
}
type Peer struct {
	SessionID         string
	ReconcileRequired bool
	conn              *websocket.Conn
	v                 *protocol.Validator
	liveness          *heartbeat
}

func validIdentity(node, ledger, token string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return idPattern.MatchString(node) && idPattern.MatchString(ledger) && err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == token
}

func NewServer(cfg Config) (*Server, error) {
	if !validIdentity(cfg.NodeID, cfg.LedgerID, cfg.Token) {
		return nil, errors.New("invalid configured identity")
	}
	v, err := protocol.NewValidator()
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, v: v, connections: make(map[*websocket.Conn]struct{})}, nil
}

func (s *Server) CurrentSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return ""
	}
	return s.current.SessionID
}

func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	connections := make([]*websocket.Conn, 0, len(s.connections))
	for c := range s.connections {
		connections = append(connections, c)
	}
	s.current = nil
	s.mu.Unlock()
	for _, c := range connections {
		c.CloseNow()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/agent/v1/connect" {
		http.NotFound(w, r)
		return
	}
	auth := r.Header.Values("Authorization")
	if r.TLS == nil || r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || len(r.Header.Values("Cookie")) != 0 || len(r.Header.Values("Origin")) != 0 || len(auth) != 1 || subtle.ConstantTimeCompare([]byte(auth[0]), []byte("Bearer "+s.cfg.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	liveness := newHeartbeat()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled, OnPongReceived: func(ctx context.Context, _ []byte) { liveness.Pong(ctx, "") }})
	if err != nil {
		return
	}
	defer c.CloseNow()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.connections[c] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.connections, c); s.mu.Unlock() }()
	c.SetReadLimit(maxMessage)
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	helloRaw, hello, err := readRaw(ctx, c, s.v)
	if err != nil {
		return
	}
	var identity struct {
		NodeID   string `json:"node_id"`
		LedgerID string `json:"ledger_id"`
	}
	if hello.Type != "hello" || json.Unmarshal(hello.Body, &identity) != nil || identity.NodeID != s.cfg.NodeID || identity.LedgerID != s.cfg.LedgerID {
		c.Close(websocket.StatusPolicyViolation, "identity_mismatch")
		return
	}
	id, err := protocol.NewID()
	if err != nil {
		return
	}
	p := &Peer{SessionID: id, ReconcileRequired: true, conn: c, v: s.v, liveness: liveness}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	old := s.current
	s.current = p
	s.mu.Unlock()
	if old != nil {
		go old.conn.Close(websocket.StatusPolicyViolation, "session_replaced")
	}
	defer func() {
		s.mu.Lock()
		if s.current == p {
			s.current = nil
		}
		s.mu.Unlock()
	}()
	welcome, err := protocol.NewMessage("welcome", id, hello.MessageID, map[string]any{"node_id": s.cfg.NodeID, "session_id": id, "reconcile_required": true})
	if err != nil {
		return
	}
	if err = c.Write(ctx, websocket.MessageText, welcome); err != nil {
		return
	}
	cancel()
	seen := map[string][32]byte{hello.MessageID: messageFingerprint(helloRaw)}
	_ = p.run(context.Background(), func(m protocol.Envelope, raw []byte) error {
		if s.CurrentSession() != id || m.SessionID == nil || *m.SessionID != id || !agentMessage(m.Type) {
			return errProtocol
		}
		hash := messageFingerprint(raw)
		if previous, ok := seen[m.MessageID]; ok && previous != hash {
			return errProtocol
		}
		// This probe has no application traffic budget beyond a bounded diagnostic window.
		if len(seen) >= 1024 {
			return errProtocol
		}
		seen[m.MessageID] = hash
		response, err := protocol.NewMessage("protocol_error", id, m.MessageID, map[string]any{"error": map[string]any{"category": "unavailable", "code": "prototype_frozen", "message": "Recovery and execution are not implemented in this probe.", "retry_action": "none"}})
		if err != nil {
			return err
		}
		writeCtx, stop := context.WithTimeout(context.Background(), handshakeTimeout)
		defer stop()
		return c.Write(writeCtx, websocket.MessageText, response)
	})
}

// All numbers in a validated envelope are schema-bounded safe integers. JSON
// normalization compares content rather than insignificant whitespace/key order.
func messageFingerprint(raw []byte) [32]byte {
	var value any
	_ = json.Unmarshal(raw, &value)
	canonical, _ := json.Marshal(value)
	return sha256.Sum256(canonical)
}

func agentMessage(kind string) bool {
	switch kind {
	case "reconcile_reply", "reconcile_completed", "ready", "request_start", "started", "result", "observation", "finalized", "protocol_error":
		return true
	}
	return false
}

func Connect(ctx context.Context, cfg ClientConfig) (*Peer, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/agent/v1/connect" || !validIdentity(cfg.NodeID, cfg.LedgerID, cfg.Token) {
		return nil, errors.New("invalid connection configuration")
	}
	v, err := protocol.NewValidator()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: cfg.RootCAs, ServerName: cfg.ServerName}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	liveness := newHeartbeat()
	c, resp, err := websocket.Dial(ctx, cfg.URL, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.Token}}, CompressionMode: websocket.CompressionDisabled, OnPongReceived: func(ctx context.Context, _ []byte) { liveness.Pong(ctx, "") }})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		return nil, errors.New("TLS or WebSocket connection failed")
	}
	ok := false
	defer func() {
		if !ok {
			c.CloseNow()
		}
	}()
	c.SetReadLimit(maxMessage)
	boot, err := protocol.NewID()
	if err != nil {
		return nil, err
	}
	endpoint, err := protocol.NewID()
	if err != nil {
		return nil, err
	}
	data, err := protocol.NewMessage("hello", "", "", map[string]any{"node_id": cfg.NodeID, "ledger_id": cfg.LedgerID, "boot_id": boot, "agent_version": "m0-transport-probe", "compose_version": "unavailable", "capabilities": []string{}, "execution_profile": map[string]any{"docker_endpoint_id": endpoint, "policy_revision": 0}})
	if err != nil {
		return nil, err
	}
	hello, err := v.Validate(data)
	if err != nil {
		return nil, err
	}
	if err = c.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, errors.New("hello write failed")
	}
	m, err := read(ctx, c, v)
	if err != nil {
		return nil, errors.New("welcome rejected")
	}
	var body struct {
		NodeID    string `json:"node_id"`
		SessionID string `json:"session_id"`
		Reconcile bool   `json:"reconcile_required"`
	}
	if m.Type != "welcome" || m.ReplyTo == nil || *m.ReplyTo != hello.MessageID || m.SessionID == nil || json.Unmarshal(m.Body, &body) != nil || body.NodeID != cfg.NodeID || body.SessionID != *m.SessionID || !body.Reconcile {
		c.Close(websocket.StatusPolicyViolation, "welcome_mismatch")
		return nil, errProtocol
	}
	ok = true
	return &Peer{SessionID: body.SessionID, ReconcileRequired: true, conn: c, v: v, liveness: liveness}, nil
}

func read(ctx context.Context, c *websocket.Conn, v *protocol.Validator) (protocol.Envelope, error) {
	_, m, err := readRaw(ctx, c, v)
	return m, err
}
func readRaw(ctx context.Context, c *websocket.Conn, v *protocol.Validator) ([]byte, protocol.Envelope, error) {
	kind, data, err := c.Read(ctx)
	if err != nil {
		return nil, protocol.Envelope{}, err
	}
	if kind != websocket.MessageText {
		c.Close(websocket.StatusPolicyViolation, "text_required")
		return nil, protocol.Envelope{}, errProtocol
	}
	if !utf8.Valid(data) {
		c.Close(websocket.StatusInvalidFramePayloadData, "invalid_utf8")
		return nil, protocol.Envelope{}, errProtocol
	}
	m, err := v.Validate(data)
	if err != nil {
		c.Close(websocket.StatusPolicyViolation, "invalid_message")
		return nil, protocol.Envelope{}, errProtocol
	}
	return data, m, nil
}

func (p *Peer) Close() { p.conn.CloseNow() }

// Run maintains the established transport. This probe never accepts execution work.
func (p *Peer) Run(ctx context.Context) error {
	return p.run(ctx, func(protocol.Envelope, []byte) error { return errProtocol })
}
func (p *Peer) run(ctx context.Context, receive func(protocol.Envelope, []byte) error) error {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.liveness.Run(ctx, p.conn)
		cancel()
	}()
	defer func() { cancel(); p.conn.CloseNow(); <-done }()
	for {
		raw, m, err := readRaw(ctx, p.conn, p.v)
		if err != nil {
			return err
		}
		if err = receive(m, raw); err != nil {
			p.conn.Close(websocket.StatusPolicyViolation, "unexpected_message")
			return errProtocol
		}
	}
}

// ParseToken accepts a file's single line; it never includes its contents in errors.
func ParseToken(data []byte) (string, error) {
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != token {
		return "", errors.New("invalid token file")
	}
	return token, nil
}
