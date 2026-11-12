package agenttransport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/agentwire"
	"docker-manager/internal/manager/operations"
	"github.com/coder/websocket"
)

func TestTLSHTTPRejectsCredentialsHeadersBodiesAndAliases(t *testing.T) {
	f := newTransportPair(t)
	client := f.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cases := []struct {
		name, method, path string
		header             http.Header
		body               string
		chunked            bool
		status             int
		code               string
	}{
		{name: "missing bearer", status: 401, code: "invalid_credentials"},
		{name: "unknown canonical secret", header: http.Header{"Authorization": {"Bearer " + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{99}, 32))}}, status: 401, code: "invalid_credentials"},
		{name: "short secret", header: http.Header{"Authorization": {"Bearer AA"}}, status: 401, code: "invalid_credentials"},
		{name: "padded secret", header: http.Header{"Authorization": {"Bearer " + f.secret + "="}}, status: 401, code: "invalid_credentials"},
		{name: "duplicate bearer", header: http.Header{"Authorization": {"Bearer " + f.secret, "Bearer " + f.secret}}, status: 401, code: "invalid_credentials"},
		{name: "extra bearer whitespace", header: http.Header{"Authorization": {"Bearer  " + f.secret}}, status: 401, code: "invalid_credentials"},
		{name: "origin", header: http.Header{"Authorization": {"Bearer " + f.secret}, "Origin": {f.server.URL}}, status: 403, code: "origin_rejected"},
		{name: "empty origin", header: http.Header{"Authorization": {"Bearer " + f.secret}, "Origin": {""}}, status: 403, code: "origin_rejected"},
		{name: "cookie", header: http.Header{"Authorization": {"Bearer " + f.secret}, "Cookie": {"unrelated=value"}}, status: 403, code: "origin_rejected"},
		{name: "empty cookie", header: http.Header{"Authorization": {"Bearer " + f.secret}, "Cookie": {""}}, status: 403, code: "origin_rejected"},
		{name: "post", method: "POST", status: 400, code: "invalid_field"},
		{name: "body", body: "x", status: 400, code: "invalid_field"},
		{name: "chunked body", body: "x", chunked: true, status: 400, code: "invalid_field"},
		{name: "query", path: "/agent/v1/connect?x=1", status: 400, code: "invalid_field"},
		{name: "empty query", path: "/agent/v1/connect?", status: 400, code: "invalid_field"},
		{name: "trailing slash", path: "/agent/v1/connect/", status: 400, code: "invalid_field"},
		{name: "escaped alias", path: "/agent/v1/%63onnect", status: 400, code: "invalid_field"},
		{name: "double slash", path: "/agent/v1//connect", status: 400, code: "invalid_field"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			method, path := test.method, test.path
			if method == "" {
				method = "GET"
			}
			if path == "" {
				path = "/agent/v1/connect"
			}
			r, err := http.NewRequestWithContext(f.ctx, method, f.server.URL+path, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			r.Header = test.header.Clone()
			if r.Header == nil {
				r.Header = http.Header{}
			}
			if test.chunked {
				r.ContentLength = -1
				r.TransferEncoding = []string{"chunked"}
			}
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(response.Body, 2048))
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Request string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &body); err != nil || response.StatusCode != test.status || body.Error.Code != test.code || body.Error.Request == "" {
				t.Fatalf("rejection status=%d body=%s error=%v", response.StatusCode, raw, err)
			}
			if !response.Close || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" || bytes.Contains(raw, []byte(f.secret)) {
				t.Fatal("rejection did not close safely or exposed credentials")
			}
		})
	}
	plain := httptest.NewServer(f.h)
	defer plain.Close()
	r, _ := http.NewRequestWithContext(f.ctx, "GET", plain.URL+"/agent/v1/connect", nil)
	r.Header.Set("Authorization", "Bearer "+f.secret)
	response, err := plain.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("plaintext accepted", response.StatusCode)
	}
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	if len(f.h.connections) != 0 || len(f.h.handshakes) != 0 {
		t.Fatal("invalid HTTP opened a connection")
	}
}

func TestTLSForeignHelloCannotReplaceCurrentSession(t *testing.T) {
	f := newTransportPair(t)
	p, b := f.dial(t)
	f.recover(t, p, b)
	for _, field := range []string{"node_id", "ledger_id"} {
		t.Run(field, func(t *testing.T) {
			var hello map[string]any
			if err := json.Unmarshal(f.hello(t, f.node), &hello); err != nil {
				t.Fatal(err)
			}
			hello["body"].(map[string]any)[field] = id(t)
			raw, _ := json.Marshal(hello)
			other, _, err := agentwire.Dial(f.ctx, agentwire.ClientConfig{URL: "wss" + strings.TrimPrefix(f.server.URL, "https") + "/agent/v1/connect", Token: f.secret, Hello: raw, RootCAs: f.server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs})
			if err == nil {
				other.Close()
				t.Fatal("foreign hello accepted")
			}
			state, err := f.m.ReadRuntimeConnectionState(f.ctx, b)
			if err != nil || state.Frozen || state.Recovery.Status != "completed" {
				t.Fatal("foreign hello damaged current session", state, err)
			}
		})
	}
}

func TestTLSReplacementClosesOldPeerWithoutClearingNewSession(t *testing.T) {
	f := newTransportPair(t)
	oldPeer, old := f.dial(t)
	f.recover(t, oldPeer, old)
	newPeer, current := f.dial(t)
	if current.SessionID == old.SessionID {
		t.Fatal("replacement reused old session")
	}
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	if _, _, err := oldPeer.Read(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("old peer was not promptly closed", err)
	}
	f.recover(t, newPeer, current)
	if _, err := f.m.ReadRuntimeConnectionState(f.ctx, old); !errors.Is(err, operations.ErrFrozen) {
		t.Fatal("old session still authoritative", err)
	}
	f.h.mu.Lock()
	if len(f.h.connections) != 1 || f.h.connections[f.node] == nil || f.h.connections[f.node].b != current {
		f.h.mu.Unlock()
		t.Fatal("replacement is not registered as sole current connection")
	}
	f.h.mu.Unlock()
	if state, err := f.m.ReadRuntimeConnectionState(f.ctx, current); err != nil || state.Frozen {
		t.Fatal("old disconnect cleared replacement session", state, err)
	}
}

func TestTLSCredentialRotationClosesOldConnectionAndRejectsOldGeneration(t *testing.T) {
	f := newTransportPair(t)
	oldPeer, b := f.dial(t)
	f.recover(t, oldPeer, b)
	oldSecret := f.secret
	newSecret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32))
	if _, err := f.m.RotateNodeCredential(f.ctx, oldSecret, operations.RotationRequest{LedgerID: b.LedgerID, CredentialSecret: newSecret}); err != nil {
		t.Fatal(err)
	}
	f.h.WakeAll()
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	if _, _, err := oldPeer.Read(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("rotated connection remained usable", err)
	}
	p, _, err := agentwire.Dial(f.ctx, agentwire.ClientConfig{URL: "wss" + strings.TrimPrefix(f.server.URL, "https") + "/agent/v1/connect", Token: oldSecret, Hello: f.hello(t, f.node), RootCAs: f.server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs})
	if err == nil {
		p.Close()
		t.Fatal("old credential generation authenticated")
	}
	if !errors.Is(err, agentwire.ErrUnauthorized) {
		t.Fatal("old generation did not fail authentication", err)
	}
	f.secret = newSecret
	newPeer, current := f.dial(t)
	f.recover(t, newPeer, current)
}

func TestTLSCloseWaitsForActiveAndHelloPendingHandlers(t *testing.T) {
	f := newTransportPair(t)
	p, b := f.dial(t)
	f.recover(t, p, b)
	// Open an authenticated, real TLS WebSocket without sending hello. The
	// transport must close this upgrade as well as the established Peer.
	partial, _, err := websocket.Dial(f.ctx, "wss"+strings.TrimPrefix(f.server.URL, "https")+"/agent/v1/connect", &websocket.DialOptions{HTTPClient: f.server.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + f.secret}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { partial.CloseNow() })
	transportEventually(t, f.ctx, "pending hello was never registered", func() bool {
		f.h.mu.Lock()
		defer f.h.mu.Unlock()
		return len(f.h.handshakes) == 1 && len(f.h.connections) == 1
	})
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	if err := f.h.Close(ctx); err != nil {
		t.Fatal("Close did not wait for handlers", err)
	}
	select {
	case <-f.h.done:
	default:
		t.Fatal("Close returned before active handlers finished")
	}
	f.h.mu.Lock()
	remaining := len(f.h.handshakes) + len(f.h.connections)
	f.h.mu.Unlock()
	if remaining != 0 {
		t.Fatal("Close left registered handlers", remaining)
	}
	if _, _, err := p.Read(ctx); err == nil {
		t.Fatal("Close left established peer open")
	}
	if _, _, err := partial.Read(ctx); err == nil {
		t.Fatal("Close left pending hello socket open")
	}
	response, err := f.server.Client().Get(f.server.URL + "/agent/v1/connect")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatal("closed handler admitted new request", response.StatusCode)
	}
	if _, err := f.m.Login(f.ctx, "admin", "transport test password"); err != nil {
		t.Fatal("transport closed the borrowed Store", err)
	}
}
