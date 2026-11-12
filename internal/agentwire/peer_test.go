package agentwire

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"github.com/coder/websocket"
)

const wireSession = "00000000-0000-4000-8000-000000000008"
const wireNode = "00000000-0000-4000-8000-000000000001"

func example(t *testing.T, kind string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/examples/agent-v1.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var examples []json.RawMessage
	if json.Unmarshal(raw, &examples) != nil {
		t.Fatal("invalid fixture")
	}
	for _, raw := range examples {
		var e protocol.Envelope
		if json.Unmarshal(raw, &e) == nil && e.Type == kind {
			return raw
		}
	}
	t.Fatal("fixture absent", kind)
	return nil
}

func wirePair(t *testing.T) (*Peer, *Peer) {
	t.Helper()
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32))
	peers := make(chan *Peer, 1)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		h, err := Accept(w, r)
		if err != nil {
			return
		}
		defer h.Close()
		raw, err := h.ReadHello(r.Context())
		if err != nil {
			return
		}
		var hello protocol.Envelope
		json.Unmarshal(raw, &hello)
		welcome, _ := protocol.NewMessage("welcome", wireSession, hello.MessageID, map[string]any{"node_id": wireNode, "session_id": wireSession, "reconcile_required": true})
		var fixed protocol.Envelope
		json.Unmarshal(welcome, &fixed)
		fixed.MessageID = "00000000-0000-4000-8000-000000000101"
		welcome, _ = json.Marshal(fixed)
		p, err := h.Welcome(r.Context(), welcome)
		if err != nil {
			return
		}
		peers <- p
		<-p.Done()
	}))
	s.StartTLS()
	config := ClientConfig{URL: "wss" + strings.TrimPrefix(s.URL, "https") + "/agent/v1/connect", Token: token, RootCAs: s.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, Hello: example(t, "hello")}
	c, welcome, err := Dial(context.Background(), config)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	var env protocol.Envelope
	if json.Unmarshal(welcome, &env) != nil || env.Type != "welcome" || c.SessionID() != wireSession {
		t.Fatal("welcome not preserved")
	}
	var manager *Peer
	select {
	case manager = <-peers:
	case <-time.After(time.Second):
		t.Fatal("server handshake did not finish")
	}
	t.Cleanup(func() { c.Close(); manager.Close(); s.Close() })
	return c, manager
}

func TestTLSHandshakeAndBidirectionalOriginalMessages(t *testing.T) {
	c, m := wirePair(t)
	for _, kind := range []string{"ready", "request_start", "started", "result", "finalized", "reconcile_reply", "reconcile_completed", "observation", "protocol_error"} {
		t.Run(kind, func(t *testing.T) {
			raw := example(t, kind)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			sent := make(chan error, 1)
			go func() { sent <- c.Write(ctx, raw) }()
			got, env, err := m.Read(ctx)
			if err != nil || env.Type != kind || !bytes.Equal(got, raw) {
				t.Fatalf("message changed %s %v", kind, err)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, kind := range []string{"prepare", "authorize_start", "deny_start", "ack", "cancel", "observe", "finalize", "reconcile", "reconcile_complete", "protocol_error"} {
		t.Run("manager_"+kind, func(t *testing.T) {
			raw := example(t, kind)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			sent := make(chan error, 1)
			go func() { sent <- m.Write(ctx, raw) }()
			got, env, err := c.Read(ctx)
			if err != nil || env.Type != kind || !bytes.Equal(got, raw) {
				t.Fatalf("Manager message changed %s %v", kind, err)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWrongSessionDirectionAndChangedIDAreRejected(t *testing.T) {
	t.Run("outgoing direction", func(t *testing.T) {
		c, _ := wirePair(t)
		if err := c.Write(context.Background(), example(t, "prepare")); !errors.Is(err, ErrProtocol) {
			t.Fatal("wrong direction accepted", err)
		}
	})
	t.Run("incoming wrong session", func(t *testing.T) {
		c, m := wirePair(t)
		raw := bytes.Replace(example(t, "ready"), []byte(wireSession), []byte("00000000-0000-4000-8000-000000000018"), 1)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		go c.conn.Write(ctx, websocket.MessageText, raw)
		if _, _, err := m.Read(ctx); !errors.Is(err, ErrProtocol) {
			t.Fatal("wrong session accepted", err)
		}
	})
	t.Run("duplicate then changed", func(t *testing.T) {
		c, m := wirePair(t)
		raw := example(t, "ready")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for i := 0; i < 2; i++ {
			go c.conn.Write(ctx, websocket.MessageText, raw)
			if _, _, err := m.Read(ctx); err != nil {
				t.Fatal("identical replay rejected", err)
			}
		}
		changed := bytes.Replace(raw, []byte(`2.0.0-example`), []byte(`2.0.1-example`), 1)
		if bytes.Equal(changed, raw) {
			t.Fatal("bad changed fixture")
		}
		go c.conn.Write(ctx, websocket.MessageText, changed)
		if _, _, err := m.Read(ctx); !errors.Is(err, ErrProtocol) {
			t.Fatal("same ID changed body accepted", err)
		}
	})
}

func TestCloseCancelsBlockedReadAndIsIdempotent(t *testing.T) {
	c, m := wirePair(t)
	done := make(chan error, 1)
	go func() { _, _, err := m.Read(context.Background()); done <- err }()
	m.Close()
	m.Close()
	c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("close left blocked reader")
	}
}

func TestReceiveIdentityBudgetAndInvalidFrames(t *testing.T) {
	t.Run("identity budget", func(t *testing.T) {
		c, m := wirePair(t)
		m.seenLimit = len(m.seen) + 2
		var base protocol.Envelope
		json.Unmarshal(example(t, "ready"), &base)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for i := 0; i < 3; i++ {
			raw, _ := protocol.NewMessage("ready", wireSession, "", base.Body)
			go c.conn.Write(ctx, websocket.MessageText, raw)
			_, _, err := m.Read(ctx)
			if i < 2 && err != nil {
				t.Fatal(err)
			}
			if i == 2 && !errors.Is(err, ErrBudget) {
				t.Fatal("budget not enforced", err)
			}
		}
	})
	for _, kind := range []string{"binary", "invalid_json", "invalid_utf8", "direction", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			c, m := wirePair(t)
			raw := example(t, "ready")
			typ := websocket.MessageText
			switch kind {
			case "binary":
				typ = websocket.MessageBinary
			case "invalid_json":
				raw = []byte(`{"protocol":1,"protocol":1}`)
			case "invalid_utf8":
				raw = []byte{255}
			case "direction":
				raw = example(t, "prepare")
			case "oversized":
				raw = bytes.Repeat([]byte{'x'}, maxMessage+1)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sent := make(chan error, 1)
			go func() { sent <- c.conn.Write(ctx, typ, raw) }()
			if _, _, err := m.Read(ctx); err == nil {
				t.Fatal("invalid frame delivered")
			}
			select {
			case <-sent:
			case <-ctx.Done():
				t.Fatal("sender did not unblock")
			}
		})
	}
}

func TestDialAuthenticationTrustAndRedirectBoundaries(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{22}, 32))
	hello := example(t, "hello")
	config := func(s *httptest.Server) ClientConfig {
		return ClientConfig{URL: "wss" + strings.TrimPrefix(s.URL, "https") + "/agent/v1/connect", Token: token, RootCAs: s.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, Hello: hello}
	}
	unauthorized := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer unauthorized.Close()
	if _, _, err := Dial(context.Background(), config(unauthorized)); !errors.Is(err, ErrUnauthorized) || strings.Contains(err.Error(), token) {
		t.Fatal("authentication error classification", err)
	}
	untrusted := config(unauthorized)
	untrusted.RootCAs = x509.NewCertPool()
	if _, _, err := Dial(context.Background(), untrusted); !errors.Is(err, ErrTransport) {
		t.Fatal("untrusted certificate accepted", err)
	}
	var calls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(401) }))
	defer destination.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/agent/v1/connect", 302)
	}))
	defer redirect.Close()
	if _, _, err := Dial(context.Background(), config(redirect)); !errors.Is(err, ErrTransport) || calls.Load() != 0 {
		t.Fatal("redirect forwarded credentials", err, calls.Load())
	}
}

func TestConcurrentWritesRemainWholeAndCanceledQueueReturns(t *testing.T) {
	c, m := wirePair(t)
	var fixture protocol.Envelope
	json.Unmarshal(example(t, "ready"), &fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			raw, _ := protocol.NewMessage("ready", wireSession, "", fixture.Body)
			errs <- c.Write(ctx, raw)
		}()
	}
	ids := map[string]bool{}
	for i := 0; i < 40; i++ {
		_, env, err := m.Read(ctx)
		if err != nil || env.Type != "ready" || ids[env.MessageID] {
			t.Fatalf("parallel frames corrupted: %v", err)
		}
		ids[env.MessageID] = true
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// A caller waiting for the single write slot must observe cancellation,
	// rather than waiting behind the current writer's entire network budget.
	<-c.writes
	defer func() { c.writes <- struct{}{} }()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := c.Write(canceled, example(t, "ready")); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled queued write blocked", err)
	}
}

func TestActualPongRefreshesPeerLiveness(t *testing.T) {
	c, m := wirePair(t)
	reads := make(chan error, 2)
	go func() { _, _, err := c.Read(context.Background()); reads <- err }()
	go func() { _, _, err := m.Read(context.Background()); reads <- err }()
	m.liveness.mu.Lock()
	m.liveness.lastPong = time.Now().Add(-40 * time.Second)
	m.liveness.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.conn.Ping(ctx); err != nil {
		t.Fatal("real control frame did not receive Pong", err)
	}
	if m.liveness.silenceRemaining() < 40*time.Second {
		t.Fatal("actual Pong did not refresh deadline")
	}
	c.Close()
	m.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-reads:
		case <-ctx.Done():
			t.Fatal("reader did not exit")
		}
	}
}

func TestHandshakeAssociationAndFirstPacketAreRequired(t *testing.T) {
	for _, kind := range []string{"welcome_node", "welcome_reply", "welcome_session", "welcome_reconcile", "first_packet", "browser_origin"} {
		t.Run(kind, func(t *testing.T) {
			observed := make(chan error, 1)
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h, err := Accept(w, r)
				if err != nil {
					observed <- err
					return
				}
				defer h.Close()
				raw, err := h.ReadHello(r.Context())
				if err != nil {
					observed <- err
					return
				}
				var hello protocol.Envelope
				json.Unmarshal(raw, &hello)
				node, reply, bodySession, reconcile := wireNode, hello.MessageID, wireSession, true
				switch kind {
				case "welcome_node":
					node = "00000000-0000-4000-8000-000000000019"
				case "welcome_reply":
					reply = "00000000-0000-4000-8000-000000000019"
				case "welcome_session":
					bodySession = "00000000-0000-4000-8000-000000000019"
				case "welcome_reconcile":
					reconcile = false
				}
				welcome, _ := protocol.NewMessage("welcome", wireSession, reply, map[string]any{"node_id": node, "session_id": bodySession, "reconcile_required": reconcile})
				_ = h.conn.Write(r.Context(), websocket.MessageText, welcome)
				observed <- nil
			}))
			s.StartTLS()
			defer s.Close()
			client := s.Client()
			token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{33}, 32))
			url := "wss" + strings.TrimPrefix(s.URL, "https") + "/agent/v1/connect"
			if kind == "first_packet" || kind == "browser_origin" {
				header := http.Header{}
				if kind == "browser_origin" {
					header.Set("Origin", s.URL)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client, HTTPHeader: header})
				if kind == "browser_origin" {
					if err == nil {
						c.CloseNow()
						t.Fatal("browser origin accepted")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					defer c.CloseNow()
					_ = c.Write(ctx, websocket.MessageText, example(t, "ready"))
				}
				if err := <-observed; !errors.Is(err, ErrProtocol) {
					t.Fatal("wrong first packet accepted", err)
				}
				return
			}
			p, _, err := Dial(context.Background(), ClientConfig{URL: url, Token: token, RootCAs: client.Transport.(*http.Transport).TLSClientConfig.RootCAs, Hello: example(t, "hello")})
			if err == nil {
				p.Close()
				t.Fatal("unassociated welcome accepted")
			}
			if !errors.Is(err, ErrProtocol) {
				t.Fatal("wrong association classification", err)
			}
			<-observed
		})
	}
}

func TestApplicationCannotReuseInboundHandshakeID(t *testing.T) {
	for _, side := range []string{"hello", "welcome"} {
		t.Run(side, func(t *testing.T) {
			c, m := wirePair(t)
			sender, receiver, kind, id := c, m, "ready", "00000000-0000-4000-8000-000000000100"
			if side == "welcome" {
				sender, receiver, kind, id = m, c, "prepare", "00000000-0000-4000-8000-000000000101"
			}
			var env protocol.Envelope
			json.Unmarshal(example(t, kind), &env)
			env.MessageID = id
			raw, _ := json.Marshal(env)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			go sender.conn.Write(ctx, websocket.MessageText, raw)
			if _, _, err := receiver.Read(ctx); !errors.Is(err, ErrProtocol) {
				t.Fatal("application reused handshake ID", err)
			}
		})
	}
}
