package channel

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"github.com/coder/websocket"
)

const acceptanceSession = "00000000-0000-4000-8000-000000000008"

func channelFixture(t *testing.T, kind, session string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/examples/agent-v1.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var examples []map[string]any
	if err = json.Unmarshal(raw, &examples); err != nil {
		t.Fatal(err)
	}
	for _, example := range examples {
		if example["type"] != kind {
			continue
		}
		if kind != "hello" {
			example["session_id"] = session
		}
		if kind == "welcome" {
			example["body"].(map[string]any)["session_id"] = session
		}
		return channelJSON(t, example)
	}
	t.Fatalf("missing published fixture for %s", kind)
	return nil
}

func channelJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func channelValidator(t *testing.T) *protocol.Validator {
	t.Helper()
	v, err := protocol.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func requireClose(t *testing.T, err error, code websocket.StatusCode, reason string) {
	t.Helper()
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != code || (reason != "" && closeErr.Reason != reason) {
		t.Fatalf("want close %d/%s, got %v", code, reason, err)
	}
}

func writeMessageFragments(w io.Writer, raw []byte) error {
	// Writer emits a frame per Write; keep each frame below the aggregate limit.
	for len(raw) > 0 {
		n := min(len(raw), 1024*1024)
		if _, err := w.Write(raw[:n]); err != nil {
			return err
		}
		raw = raw[n:]
	}
	return nil
}

// A real TLS peer permits sending wire inputs Connect must reject, without
// adding a test-only bypass to the production Manager or Agent.
func scriptedManager(t *testing.T, beforeUpgrade time.Duration, script func(context.Context, *websocket.Conn, protocol.Envelope) error) (ClientConfig, <-chan error) {
	t.Helper()
	v := channelValidator(t)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if beforeUpgrade > 0 {
			select {
			case <-time.After(beforeUpgrade):
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			done <- err
			return
		}
		defer c.CloseNow()
		_, raw, err := c.Read(ctx)
		if err != nil {
			done <- err
			return
		}
		hello, err := v.Validate(raw)
		if err != nil || hello.Type != "hello" {
			done <- fmt.Errorf("expected valid hello, got %q: %v", hello.Type, err)
			return
		}
		done <- script(ctx, c, hello)
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(cancel)
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	return ClientConfig{URL: strings.Replace(ts.URL, "https:", "wss:", 1) + "/agent/v1/connect", NodeID: nodeID, LedgerID: ledgerID, Token: secret, RootCAs: roots}, done
}

func writeWelcome(ctx context.Context, c *websocket.Conn, hello protocol.Envelope) error {
	raw := []byte(`{"protocol":1,"type":"welcome","message_id":"00000000-0000-4000-8000-000000000101","session_id":"` + acceptanceSession + `","reply_to":"` + hello.MessageID + `","body":{"node_id":"` + nodeID + `","session_id":"` + acceptanceSession + `","reconcile_required":true}}`)
	return c.Write(ctx, websocket.MessageText, raw)
}

func TestUpgradeBoundaryResponses(t *testing.T) {
	for _, tc := range []struct {
		name, path, method string
		plain              bool
		want               int
	}{
		{"wrong_path", "/agent/v1/other", "GET", false, 404},
		{"trailing_slash", "/agent/v1/connect/", "GET", false, 404},
		{"wrong_method", "/agent/v1/connect", "POST", false, 401},
		{"empty_query", "/agent/v1/connect?", "GET", false, 401},
		{"nonempty_query", "/agent/v1/connect?x=y", "GET", false, 401},
		{"plaintext", "/agent/v1/connect", "GET", true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ts, _ := setup(t)
			if tc.plain {
				ts = httptest.NewServer(s)
				defer ts.Close()
			}
			req, err := http.NewRequestWithContext(deadline(t), tc.method, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = http.Header{"Authorization": {"Bearer " + secret}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("want HTTP %d/no-store, got %d/%q", tc.want, resp.StatusCode, resp.Header.Get("Cache-Control"))
			}
			if s.CurrentSession() != "" {
				t.Fatal("rejected Upgrade established an application session")
			}
		})
	}
}

func TestManagerDoesNotNegotiateCompression(t *testing.T) {
	_, ts, cfg := setup(t)
	c, resp, err := websocket.Dial(deadline(t), cfg.URL, &websocket.DialOptions{HTTPClient: ts.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}, CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected Upgrade headers: %v", resp.Header)
	}
	if err = c.Write(deadline(t), websocket.MessageText, helloForTest()); err != nil {
		t.Fatal(err)
	}
	_, raw, err := c.Read(deadline(t))
	if err != nil {
		t.Fatal(err)
	}
	m, err := channelValidator(t).Validate(raw)
	if err != nil || m.Type != "welcome" {
		t.Fatalf("compression-disabled Upgrade did not establish a session: %v", err)
	}
}

func TestAgentDoesNotOfferCompression(t *testing.T) {
	header := make(chan string, 1)
	s, _, _ := setup(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header <- r.Header.Get("Sec-WebSocket-Extensions")
		s.ServeHTTP(w, r)
	}))
	defer ts.Close()
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	cfg := ClientConfig{URL: strings.Replace(ts.URL, "https:", "wss:", 1) + "/agent/v1/connect", NodeID: nodeID, LedgerID: ledgerID, Token: secret, RootCAs: roots}
	p, err := Connect(deadline(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := <-header; got != "" {
		t.Fatalf("Agent offered WebSocket extensions: %q", got)
	}
}

func TestAgentNeverFollowsRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var redirects, targets atomic.Int32
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					targets.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				redirects.Add(1)
				http.Redirect(w, r, "/target", status)
			}))
			defer ts.Close()
			roots := x509.NewCertPool()
			roots.AddCert(ts.Certificate())
			cfg := ClientConfig{URL: strings.Replace(ts.URL, "https:", "wss:", 1) + "/agent/v1/connect", NodeID: nodeID, LedgerID: ledgerID, Token: secret, RootCAs: roots}
			p, err := Connect(deadline(t), cfg)
			if p != nil {
				p.Close()
			}
			if err == nil || redirects.Load() != 1 || targets.Load() != 0 {
				t.Fatalf("redirect result: error=%v initial=%d target=%d", err, redirects.Load(), targets.Load())
			}
		})
	}
}

func TestHandshakeDeadlinesOverTLS(t *testing.T) {
	t.Run("manager_hello_ten_seconds", func(t *testing.T) {
		t.Parallel()
		s, ts, cfg := setup(t)
		ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, cfg.URL, &websocket.DialOptions{HTTPClient: ts.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseNow()
		start := time.Now()
		_, _, err = c.Read(ctx)
		elapsed := time.Since(start)
		if err == nil || ctx.Err() != nil || elapsed < 9*time.Second || elapsed > 13*time.Second || s.CurrentSession() != "" {
			t.Fatalf("hello deadline: elapsed=%v error=%v context=%v session=%q", elapsed, err, ctx.Err(), s.CurrentSession())
		}
	})
	for _, tc := range []struct {
		name                string
		parent, upgradeWait time.Duration
		min, max            time.Duration
	}{
		{"agent_welcome_ten_seconds", 14 * time.Second, 0, 9 * time.Second, 13 * time.Second},
		{"agent_deadline_includes_dial", 14 * time.Second, 2 * time.Second, 9 * time.Second, 11 * time.Second},
		{"agent_earlier_parent", 350 * time.Millisecond, 0, 250 * time.Millisecond, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var receivedHello atomic.Bool
			cfg, done := scriptedManager(t, tc.upgradeWait, func(ctx context.Context, c *websocket.Conn, _ protocol.Envelope) error {
				receivedHello.Store(true)
				_, _, err := c.Read(ctx)
				return err
			})
			ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
			defer cancel()
			start := time.Now()
			p, err := Connect(ctx, cfg)
			elapsed := time.Since(start)
			if p != nil {
				p.Close()
			}
			if err == nil || !receivedHello.Load() || elapsed < tc.min || elapsed > tc.max {
				t.Fatalf("welcome deadline: elapsed=%v error=%v hello=%v", elapsed, err, receivedHello.Load())
			}
			if err := <-done; err == nil {
				t.Fatal("Agent left waiting-welcome connection open")
			}
		})
	}
}

func TestWelcomeCorrelationAndEmptyCapabilities(t *testing.T) {
	for _, name := range []string{"valid", "wrong_type", "wrong_node", "wrong_reply_to", "wrong_envelope_session", "wrong_body_session", "reconcile_false"} {
		t.Run(name, func(t *testing.T) {
			v := channelValidator(t)
			cfg, done := scriptedManager(t, 0, func(ctx context.Context, c *websocket.Conn, hello protocol.Envelope) error {
				var body map[string]any
				if err := json.Unmarshal(hello.Body, &body); err != nil {
					return err
				}
				capabilities, ok := body["capabilities"].([]any)
				if !ok || len(capabilities) != 0 || body["agent_version"] != "m0-transport-probe" || body["compose_version"] != "unavailable" {
					return fmt.Errorf("Agent advertised executable capability: %s", hello.Body)
				}
				welcome := map[string]any{"protocol": 1, "type": "welcome", "message_id": "00000000-0000-4000-8000-000000000101", "session_id": acceptanceSession, "reply_to": hello.MessageID, "body": map[string]any{"node_id": nodeID, "session_id": acceptanceSession, "reconcile_required": true}}
				b := welcome["body"].(map[string]any)
				switch name {
				case "wrong_type":
					welcome["type"] = "ack"
					welcome["body"] = map[string]any{"operation_id": "00000000-0000-4000-8000-000000000004", "event_seq": 1}
				case "wrong_node":
					b["node_id"] = "00000000-0000-4000-8000-000000000002"
				case "wrong_reply_to":
					welcome["reply_to"] = "00000000-0000-4000-8000-000000000099"
				case "wrong_envelope_session":
					welcome["session_id"] = "00000000-0000-4000-8000-000000000007"
				case "wrong_body_session":
					b["session_id"] = "00000000-0000-4000-8000-000000000007"
				case "reconcile_false":
					b["reconcile_required"] = false
				}
				raw, err := json.Marshal(welcome)
				if err != nil {
					return err
				}
				if _, err = v.Validate(raw); err != nil {
					return fmt.Errorf("welcome case must pass schema before correlation check: %w", err)
				}
				if err = c.Write(ctx, websocket.MessageText, raw); err != nil {
					return err
				}
				_, _, err = c.Read(ctx)
				return err
			})
			p, err := Connect(deadline(t), cfg)
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if p.SessionID != acceptanceSession || !p.ReconcileRequired {
					t.Fatalf("unexpected accepted welcome: %+v", p)
				}
				p.Close()
				<-done
				return
			}
			if p != nil {
				p.Close()
			}
			if !errors.Is(err, errProtocol) {
				t.Fatalf("expected welcome correlation rejection, got %v", err)
			}
			requireClose(t, <-done, websocket.StatusPolicyViolation, "welcome_mismatch")
		})
	}
}

func assertFrozenResponse(t *testing.T, c *websocket.Conn, v *protocol.Validator, raw []byte, session string) {
	t.Helper()
	request, err := v.Validate(raw)
	if err != nil {
		t.Fatalf("invalid request fixture: %v", err)
	}
	ctx := deadline(t)
	if err = c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	kind, response, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Validate(response)
	if err != nil {
		t.Fatalf("invalid frozen response: %v", err)
	}
	if kind != websocket.MessageText || got.Protocol != 1 || got.Type != "protocol_error" || got.SessionID == nil || *got.SessionID != session || got.ReplyTo == nil || *got.ReplyTo != request.MessageID || got.MessageID == request.MessageID {
		t.Fatalf("incorrect frozen envelope: %s", response)
	}
	var body any
	if err = json.Unmarshal(got.Body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"error": map[string]any{"category": "unavailable", "code": "prototype_frozen", "message": "Recovery and execution are not implemented in this probe.", "retry_action": "none"}}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("incorrect frozen diagnostic: %s", got.Body)
	}
}

func TestManagerFrozenMessageSupportMatrix(t *testing.T) {
	for _, kind := range []string{"reconcile_reply", "reconcile_completed", "ready", "request_start", "started", "result", "observation", "finalized", "protocol_error"} {
		t.Run(kind, func(t *testing.T) {
			_, ts, cfg := setup(t)
			c, session := rawHandshake(t, ts, cfg)
			assertFrozenResponse(t, c, channelValidator(t), channelFixture(t, kind, session), session)
		})
	}
}

func TestManagerRejectsEveryReverseDirection(t *testing.T) {
	for _, kind := range []string{"welcome", "reconcile", "reconcile_complete", "prepare", "authorize_start", "deny_start", "ack", "cancel", "observe", "finalize"} {
		t.Run(kind, func(t *testing.T) {
			_, ts, cfg := setup(t)
			c, session := rawHandshake(t, ts, cfg)
			raw := channelFixture(t, kind, session)
			if _, err := channelValidator(t).Validate(raw); err != nil {
				t.Fatalf("wrong-direction fixture must otherwise be valid: %v", err)
			}
			if err := c.Write(deadline(t), websocket.MessageText, raw); err != nil {
				t.Fatal(err)
			}
			_, _, err := c.Read(deadline(t))
			requireClose(t, err, websocket.StatusPolicyViolation, "unexpected_message")
		})
	}
}

func TestAgentRejectsEveryApplicationMessage(t *testing.T) {
	for _, kind := range []string{"hello", "welcome", "reconcile", "reconcile_reply", "reconcile_complete", "reconcile_completed", "prepare", "ready", "request_start", "authorize_start", "deny_start", "started", "result", "ack", "cancel", "observe", "observation", "finalize", "finalized", "protocol_error"} {
		t.Run(kind, func(t *testing.T) {
			raw := channelFixture(t, kind, acceptanceSession)
			if _, err := channelValidator(t).Validate(raw); err != nil {
				t.Fatalf("application fixture must pass shared validation: %v", err)
			}
			cfg, done := scriptedManager(t, 0, func(ctx context.Context, c *websocket.Conn, hello protocol.Envelope) error {
				if err := writeWelcome(ctx, c, hello); err != nil {
					return err
				}
				if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
					return err
				}
				_, _, err := c.Read(ctx)
				return err
			})
			p, err := Connect(deadline(t), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err = p.Run(deadline(t)); !errors.Is(err, errProtocol) {
				t.Fatalf("Agent accepted application message: %v", err)
			}
			requireClose(t, <-done, websocket.StatusPolicyViolation, "unexpected_message")
		})
	}
}

func TestFingerprintBudgetIncludesHelloAndDoesNotGrowOnRetry(t *testing.T) {
	for _, retryAtCapacity := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry_at_capacity_%v", retryAtCapacity), func(t *testing.T) {
			_, ts, cfg := setup(t)
			c, session := rawHandshake(t, ts, cfg)
			v := channelValidator(t)
			var last []byte
			for i := 0; i < 1023; i++ {
				last = requestForTest(session, fmt.Sprintf("10000000-0000-4000-8000-%012d", i))
				assertFrozenResponse(t, c, v, last, session)
				if i == 0 || i == 1021 {
					// Repeated retries before the last free slot may not consume it.
					for retry := 0; retry < 3; retry++ {
						assertFrozenResponse(t, c, v, last, session)
					}
				}
			}
			if !retryAtCapacity {
				last = requestForTest(session, "10000000-0000-4000-8000-000000001023")
			}
			if err := c.Write(deadline(t), websocket.MessageText, last); err != nil {
				t.Fatal(err)
			}
			_, _, err := c.Read(deadline(t))
			requireClose(t, err, websocket.StatusPolicyViolation, "unexpected_message")
		})
	}
}

func TestBusinessMessageIDCannotChangeContent(t *testing.T) {
	_, ts, cfg := setup(t)
	c, session := rawHandshake(t, ts, cfg)
	v := channelValidator(t)
	raw := requestForTest(session, "00000000-0000-4000-8000-000000000102")
	assertFrozenResponse(t, c, v, raw, session)
	raw = bytes.Replace(raw, []byte(`"start_nonce":"00000000-0000-4000-8000-000000000011"`), []byte(`"start_nonce":"00000000-0000-4000-8000-000000000012"`), 1)
	if _, err := v.Validate(raw); err != nil {
		t.Fatalf("changed-content fixture must remain valid: %v", err)
	}
	if err := c.Write(deadline(t), websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.Read(deadline(t))
	requireClose(t, err, websocket.StatusPolicyViolation, "unexpected_message")
}

func TestAgentRejectsMalformedWireMessages(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		code         websocket.StatusCode
	}{
		{"binary", "text_required", websocket.StatusPolicyViolation},
		{"invalid_utf8", "invalid_utf8", websocket.StatusInvalidFramePayloadData},
		{"duplicate_key", "invalid_message", websocket.StatusPolicyViolation},
		{"trailing_json", "invalid_message", websocket.StatusPolicyViolation},
		{"unknown_field", "invalid_message", websocket.StatusPolicyViolation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := channelFixture(t, "ack", acceptanceSession)
			kind := websocket.MessageText
			switch tc.name {
			case "binary":
				kind = websocket.MessageBinary
			case "invalid_utf8":
				raw = []byte{0xff}
			case "duplicate_key":
				raw = bytes.Replace(raw, []byte(`"protocol":1`), []byte(`"protocol":1,"protocol":1`), 1)
			case "trailing_json":
				raw = append(raw, []byte(`{}`)...)
			case "unknown_field":
				raw = bytes.Replace(raw, []byte(`"protocol":1`), []byte(`"protocol":1,"private_secret":"do-not-echo"`), 1)
			}
			cfg, done := scriptedManager(t, 0, func(ctx context.Context, c *websocket.Conn, hello protocol.Envelope) error {
				if err := writeWelcome(ctx, c, hello); err != nil {
					return err
				}
				if err := c.Write(ctx, kind, raw); err != nil {
					return err
				}
				_, _, err := c.Read(ctx)
				return err
			})
			p, err := Connect(deadline(t), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err = p.Run(deadline(t)); err == nil {
				t.Fatal("malformed message accepted")
			}
			requireClose(t, <-done, tc.code, tc.reason)
		})
	}
}

func TestAgentFragmentedMessageBudget(t *testing.T) {
	for _, size := range []int{8*1024*1024 - 1, 8 * 1024 * 1024, 8*1024*1024 + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			raw := channelFixture(t, "ack", acceptanceSession)
			raw = append(raw, bytes.Repeat([]byte(" "), size-len(raw))...)
			cfg, done := scriptedManager(t, 0, func(ctx context.Context, c *websocket.Conn, hello protocol.Envelope) error {
				if err := writeWelcome(ctx, c, hello); err != nil {
					return err
				}
				closed := make(chan error, 1)
				go func() { _, _, err := c.Read(ctx); closed <- err }()
				w, err := c.Writer(ctx, websocket.MessageText)
				if err != nil {
					return err
				}
				_ = writeMessageFragments(w, raw)
				_ = w.Close()
				return <-closed
			})
			p, err := Connect(deadline(t), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err = p.Run(deadline(t)); err == nil {
				t.Fatal("Agent accepted post-handshake application message")
			}
			if size > 8*1024*1024 {
				requireClose(t, <-done, websocket.StatusMessageTooBig, "")
			} else {
				// Valid JSON at/below the reassembled limit reaches the application
				// rejection, proving it was not rejected by an undersized budget.
				requireClose(t, <-done, websocket.StatusPolicyViolation, "unexpected_message")
			}
		})
	}
}

func TestManagerFragmentedMessageBudgetBoundaries(t *testing.T) {
	for _, size := range []int{8*1024*1024 - 1, 8 * 1024 * 1024, 8*1024*1024 + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			_, ts, cfg := setup(t)
			c, session := rawHandshake(t, ts, cfg)
			raw := requestForTest(session, "00000000-0000-4000-8000-000000000102")
			raw = append(raw, bytes.Repeat([]byte(" "), size-len(raw))...)
			type result struct {
				raw []byte
				err error
			}
			ctx := deadline(t)
			read := make(chan result, 1)
			go func() { _, raw, err := c.Read(ctx); read <- result{raw, err} }()
			w, err := c.Writer(ctx, websocket.MessageText)
			if err != nil {
				t.Fatal(err)
			}
			writeErr := writeMessageFragments(w, raw)
			closeErr := w.Close()
			got := <-read
			if size > 8*1024*1024 {
				requireClose(t, got.err, websocket.StatusMessageTooBig, "")
				return
			}
			if got.err != nil || writeErr != nil || closeErr != nil {
				t.Fatalf("within-budget fragmented message rejected: read=%v write=%v close=%v", got.err, writeErr, closeErr)
			}
			m, err := channelValidator(t).Validate(got.raw)
			if err != nil || m.Type != "protocol_error" || m.ReplyTo == nil || *m.ReplyTo != "00000000-0000-4000-8000-000000000102" {
				t.Fatalf("within-budget input did not reach frozen response: %s / %v", got.raw, err)
			}
		})
	}
}

func TestAdvertisedCapabilitiesDoNotUnfreezeManager(t *testing.T) {
	_, ts, cfg := setup(t)
	c, _, err := websocket.Dial(deadline(t), cfg.URL, &websocket.DialOptions{HTTPClient: ts.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	raw := channelFixture(t, "hello", "")
	v := channelValidator(t)
	hello, err := v.Validate(raw)
	if err != nil {
		t.Fatal(err)
	}
	var advertised struct {
		Capabilities []string `json:"capabilities"`
	}
	if err = json.Unmarshal(hello.Body, &advertised); err != nil || len(advertised.Capabilities) == 0 {
		t.Fatalf("fixture must advertise execution capabilities: %v", err)
	}
	if err = c.Write(deadline(t), websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err = c.Read(deadline(t))
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := v.Validate(raw)
	if err != nil || welcome.Type != "welcome" || welcome.SessionID == nil {
		t.Fatalf("invalid welcome: %s / %v", raw, err)
	}
	var body struct {
		Reconcile bool `json:"reconcile_required"`
	}
	if err = json.Unmarshal(welcome.Body, &body); err != nil || !body.Reconcile {
		t.Fatalf("advertised capability bypassed required reconciliation: %s / %v", raw, err)
	}
	assertFrozenResponse(t, c, v, requestForTest(*welcome.SessionID, "00000000-0000-4000-8000-000000000102"), *welcome.SessionID)
}

func TestInitialHelloIdentityCloseReasons(t *testing.T) {
	for _, name := range []string{"wrong_node", "wrong_ledger", "not_hello"} {
		t.Run(name, func(t *testing.T) {
			s, ts, cfg := setup(t)
			c, _, err := websocket.Dial(deadline(t), cfg.URL, &websocket.DialOptions{HTTPClient: ts.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			raw := helloForTest()
			switch name {
			case "wrong_node":
				raw = bytes.Replace(raw, []byte(nodeID), []byte("00000000-0000-4000-8000-000000000002"), 1)
			case "wrong_ledger":
				raw = bytes.Replace(raw, []byte(ledgerID), []byte("00000000-0000-4000-8000-000000000007"), 1)
			case "not_hello":
				raw = channelFixture(t, "ack", acceptanceSession)
			}
			if _, err = channelValidator(t).Validate(raw); err != nil {
				t.Fatalf("identity fixture must otherwise pass validation: %v", err)
			}
			if err = c.Write(deadline(t), websocket.MessageText, raw); err != nil {
				t.Fatal(err)
			}
			_, _, err = c.Read(deadline(t))
			requireClose(t, err, websocket.StatusPolicyViolation, "identity_mismatch")
			if s.CurrentSession() != "" {
				t.Fatal("rejected hello established a session")
			}
		})
	}
}
