package agenttransport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"docker-manager/internal/agentwire"
	"docker-manager/internal/manager/operations"
	"docker-manager/internal/protocol"
)

var ErrClosed = errors.New("agent transport closed")

type Handler struct {
	store       *operations.Store
	mu          sync.Mutex
	closed      bool
	connections map[string]*connection
	handshakes  map[*agentwire.Handshake]bool
	active      sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

func New(s *operations.Store) (*Handler, error) {
	if s == nil {
		return nil, operations.ErrInput
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Handler{store: s, ctx: ctx, cancel: cancel, connections: map[string]*connection{}, handshakes: map[*agentwire.Handshake]bool{}, done: make(chan struct{})}, nil
}

func reject(w http.ResponseWriter, code string, status int) {
	id, _ := protocol.NewID()
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	category, retry := "permission_denied", "none"
	if status == 400 {
		category = "invalid_input"
	}
	if status >= 500 {
		category = "unavailable"
		retry = "same_request"
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"category": category, "code": code, "message": "节点连接未完成，请核对身份与当前状态。", "request_id": id, "field_errors": []any{}, "retry_action": retry}})
}

func bearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Split(values[0], " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	defer clear(raw)
	return parts[1], err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == parts[1]
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		reject(w, "storage_busy", 503)
		return
	}
	h.active.Add(1)
	h.mu.Unlock()
	defer h.active.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	defer cancel()
	r = r.WithContext(ctx)
	if r.TLS == nil || r.Method != "GET" || r.URL.Path != "/agent/v1/connect" || r.URL.EscapedPath() != r.URL.Path || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		reject(w, "invalid_field", 400)
		return
	}
	if len(r.Header.Values("Origin")) != 0 || len(r.Header.Values("Cookie")) != 0 {
		reject(w, "origin_rejected", 403)
		return
	}
	token, ok := bearer(r)
	if !ok {
		reject(w, "invalid_credentials", 401)
		return
	}
	auth, err := h.store.AuthenticateNodeCredential(ctx, token)
	token = ""
	if err != nil {
		var e *operations.ServiceError
		if errors.As(err, &e) {
			reject(w, e.Code, e.Status)
		} else {
			reject(w, "internal_error", 500)
		}
		return
	}
	hs, err := acceptAgentUpgrade(w, r)
	if err != nil {
		return
	}
	defer hs.Close()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.handshakes[hs] = true
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.handshakes, hs); h.mu.Unlock() }()
	raw, err := hs.ReadHello(ctx)
	if err != nil {
		return
	}
	b, welcome, err := h.store.BindAgentHello(ctx, auth, raw)
	if err != nil {
		return
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5e9)
		defer done()
		_ = h.store.DisconnectNodeSession(cleanup, auth, b.SessionID)
	}()
	p, err := hs.Welcome(ctx, welcome)
	if err != nil {
		return
	}
	c := newConnection(h, ctx, auth, b, p)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.stop()
		return
	}
	// Welcome may finish after a newer handshake has already bound and
	// registered. Keep the registry lock from current-binding validation through
	// replacement, so this late handler cannot stop the newer registered peer.
	if _, err := h.store.ReadRuntimeConnectionState(ctx, b); err != nil {
		h.mu.Unlock()
		c.stop()
		return
	}
	old := h.connections[b.NodeID]
	h.connections[b.NodeID] = c
	delete(h.handshakes, hs)
	h.mu.Unlock()
	if old != nil {
		old.stop()
	}
	defer func() {
		h.mu.Lock()
		if h.connections[b.NodeID] == c {
			delete(h.connections, b.NodeID)
		}
		h.mu.Unlock()
	}()
	c.run()
}

// WakeAll is a scheduling hint after browser mutations. Store transactions and
// their persisted next_attempt remain authoritative even when hints are lost.
func (h *Handler) WakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.connections {
		c.wake()
	}
}

func (h *Handler) Close(ctx context.Context) error {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		h.cancel()
		for hs := range h.handshakes {
			hs.Close()
		}
		for _, c := range h.connections {
			c.stop()
		}
		go func() { h.active.Wait(); close(h.done) }()
	}
	h.mu.Unlock()
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
