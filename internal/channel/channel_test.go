package channel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const nodeID = "00000000-0000-4000-8000-000000000001"
const ledgerID = "00000000-0000-4000-8000-000000000005"
const secret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func setup(t *testing.T) (*Server, *httptest.Server, ClientConfig) {
	t.Helper()
	s, err := NewServer(Config{NodeID: nodeID, LedgerID: ledgerID, Token: secret})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s)
	ts.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(func() { s.Close(); ts.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	return s, ts, ClientConfig{URL: strings.Replace(ts.URL, "https:", "wss:", 1) + "/agent/v1/connect", NodeID: nodeID, LedgerID: ledgerID, Token: secret, RootCAs: roots}
}

func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestTrustedHandshakeRemainsFrozen(t *testing.T) {
	_, _, cfg := setup(t)
	p, err := Connect(deadline(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.SessionID == "" || !p.ReconcileRequired {
		t.Fatal("prototype must allocate a session and remain frozen")
	}
}

func TestTLSAndIdentityRejection(t *testing.T) {
	for _, name := range []string{"untrusted_ca", "wrong_hostname", "wrong_token", "wrong_node", "wrong_ledger", "insecure_scheme", "url_secret"} {
		t.Run(name, func(t *testing.T) {
			_, _, cfg := setup(t)
			switch name {
			case "untrusted_ca":
				cfg.RootCAs = x509.NewCertPool()
			case "wrong_hostname":
				cfg.ServerName = "wrong.example.invalid"
			case "wrong_token":
				cfg.Token = strings.Repeat("B", 42) + "A"
			case "wrong_node":
				cfg.NodeID = "00000000-0000-4000-8000-000000000002"
			case "wrong_ledger":
				cfg.LedgerID = "00000000-0000-4000-8000-000000000006"
			case "insecure_scheme":
				cfg.URL = strings.Replace(cfg.URL, "wss:", "ws:", 1)
			case "url_secret":
				cfg.URL += "?token=" + secret
			}
			p, err := Connect(deadline(t), cfg)
			if p != nil {
				p.Close()
			}
			if err == nil {
				t.Fatal("invalid connection accepted")
			}
			if name == "wrong_token" && !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("401 must be classified as terminal: %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("secret exposed in error")
			}
		})
	}
}

func TestUpgradeRejectsAlternateCredentials(t *testing.T) {
	for _, name := range []string{"cookie", "origin", "query", "duplicate_authorization"} {
		t.Run(name, func(t *testing.T) {
			_, ts, cfg := setup(t)
			h := http.Header{"Authorization": {"Bearer " + secret}}
			switch name {
			case "cookie":
				h.Set("Cookie", "session=anything")
			case "origin":
				h.Set("Origin", ts.URL)
			case "query":
				cfg.URL += "?token=x"
			case "duplicate_authorization":
				h.Add("Authorization", "Bearer "+secret)
			}
			c, resp, err := websocket.Dial(deadline(t), cfg.URL, &websocket.DialOptions{HTTPClient: ts.Client(), HTTPHeader: h})
			if c != nil {
				c.CloseNow()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected HTTP 401, got %v %v", resp, err)
			}
		})
	}
}

func TestSessionReplacementClosesOldConnection(t *testing.T) {
	s, _, cfg := setup(t)
	first, err := Connect(deadline(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Connect(deadline(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.SessionID == second.SessionID || !second.ReconcileRequired {
		t.Fatal("new session was not isolated")
	}
	_, _, err = first.conn.Read(deadline(t))
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("old session not replaced: %v", err)
	}
	if got := s.CurrentSession(); got != second.SessionID {
		t.Fatalf("old cleanup removed new session: %s", got)
	}
}

func TestPostHandshakeInvalidMessagesClose(t *testing.T) {
	for _, name := range []string{"duplicate_hello", "wrong_direction", "old_session", "binary", "invalid_utf8", "duplicate_json_key", "oversized"} {
		t.Run(name, func(t *testing.T) {
			_, _, cfg := setup(t)
			p, err := Connect(deadline(t), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			kind := websocket.MessageText
			want := websocket.StatusPolicyViolation
			body := []byte(`{"protocol":1,"type":"request_start","message_id":"00000000-0000-4000-8000-000000000100","session_id":"` + p.SessionID + `","reply_to":null,"body":{"operation_id":"00000000-0000-4000-8000-000000000004","input_digest":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","start_nonce":"00000000-0000-4000-8000-000000000011"}}`)
			switch name {
			case "duplicate_hello":
				body = helloForTest()
			case "wrong_direction":
				body = []byte(`{"protocol":1,"type":"ack","message_id":"00000000-0000-4000-8000-000000000100","session_id":"` + p.SessionID + `","reply_to":"00000000-0000-4000-8000-000000000101","body":{"operation_id":"00000000-0000-4000-8000-000000000004","event_seq":1}}`)
			case "old_session":
				body = []byte(strings.Replace(string(body), p.SessionID, "00000000-0000-4000-8000-000000000008", 1))
			case "binary":
				kind = websocket.MessageBinary
			case "invalid_utf8":
				body = []byte{0xff}
				want = websocket.StatusInvalidFramePayloadData
			case "duplicate_json_key":
				body = bytes.Replace(body, []byte(`"protocol":1`), []byte(`"protocol":1,"protocol":1`), 1)
			case "oversized":
				body = []byte(strings.Repeat(" ", 8*1024*1024+1))
				want = websocket.StatusMessageTooBig
			}
			_ = p.conn.Write(deadline(t), kind, body)
			_, _, err = p.conn.Read(deadline(t))
			if websocket.CloseStatus(err) != want {
				t.Fatalf("want close %d, got %v", want, err)
			}
		})
	}
}

func TestFrozenSessionReturnsErrorWithoutStarting(t *testing.T) {
	_, _, cfg := setup(t)
	p, err := Connect(deadline(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	msg := []byte(`{"protocol":1,"type":"request_start","message_id":"00000000-0000-4000-8000-000000000100","session_id":"` + p.SessionID + `","reply_to":null,"body":{"operation_id":"00000000-0000-4000-8000-000000000004","input_digest":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","start_nonce":"00000000-0000-4000-8000-000000000011"}}`)
	if err = p.conn.Write(deadline(t), websocket.MessageText, msg); err != nil {
		t.Fatal(err)
	}
	_, data, err := p.conn.Read(deadline(t))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type  string `json:"type"`
		Reply string `json:"reply_to"`
	}
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "protocol_error" || got.Reply != "00000000-0000-4000-8000-000000000100" {
		t.Fatalf("unexpected frozen response: %s", data)
	}
}

func helloForTest() []byte {
	return []byte(`{"protocol":1,"type":"hello","message_id":"00000000-0000-4000-8000-000000000100","session_id":null,"reply_to":null,"body":{"node_id":"` + nodeID + `","ledger_id":"` + ledgerID + `","boot_id":"00000000-0000-4000-8000-000000000006","agent_version":"m0-probe","compose_version":"unavailable","capabilities":[],"execution_profile":{"docker_endpoint_id":"00000000-0000-4000-8000-000000000009","policy_revision":0}}}`)
}

func rawHandshake(t *testing.T, ts *httptest.Server, cfg ClientConfig) (*websocket.Conn, string) {
	t.Helper()
	c, _, err := websocket.Dial(deadline(t), cfg.URL, &websocket.DialOptions{HTTPClient: ts.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	if err = c.Write(deadline(t), websocket.MessageText, helloForTest()); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(deadline(t))
	if err != nil {
		t.Fatal(err)
	}
	var welcome struct {
		Session string `json:"session_id"`
	}
	if err = json.Unmarshal(data, &welcome); err != nil {
		t.Fatal(err)
	}
	return c, welcome.Session
}

func requestForTest(session, id string) []byte {
	return []byte(`{"protocol":1,"type":"request_start","message_id":"` + id + `","session_id":"` + session + `","reply_to":null,"body":{"operation_id":"00000000-0000-4000-8000-000000000004","input_digest":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","start_nonce":"00000000-0000-4000-8000-000000000011"}}`)
}

func TestHelloMessageIDCannotBeReused(t *testing.T) {
	_, ts, cfg := setup(t)
	c, session := rawHandshake(t, ts, cfg)
	if err := c.Write(deadline(t), websocket.MessageText, requestForTest(session, "00000000-0000-4000-8000-000000000100")); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.Read(deadline(t))
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("hello ID reuse accepted: %v", err)
	}
}

func TestEquivalentJSONRetryKeepsSameMeaning(t *testing.T) {
	_, ts, cfg := setup(t)
	c, session := rawHandshake(t, ts, cfg)
	raw := requestForTest(session, "00000000-0000-4000-8000-000000000102")
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{raw, pretty} {
		if err = c.Write(deadline(t), websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
		_, reply, readErr := c.Read(deadline(t))
		if readErr != nil {
			t.Fatalf("same message rejected after JSON formatting change: %v", readErr)
		}
		if !bytes.Contains(reply, []byte(`"type":"protocol_error"`)) {
			t.Fatalf("unexpected response: %s", reply)
		}
	}
}

func TestServerCloseIncludesPendingHello(t *testing.T) {
	s, ts, cfg := setup(t)
	c, _, err := websocket.Dial(deadline(t), cfg.URL, &websocket.DialOptions{HTTPClient: ts.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	s.Close()
	_, _, err = c.Read(deadline(t))
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending handshake was not closed: %v", err)
	}
}

func TestFragmentedMessageBudget(t *testing.T) {
	_, _, cfg := setup(t)
	p, err := Connect(deadline(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	closed := make(chan error, 1)
	go func() { _, _, err := p.conn.Read(deadline(t)); closed <- err }()
	w, err := p.conn.Writer(deadline(t), websocket.MessageText)
	if err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat(" ", 1024*1024)
	for i := 0; i < 9; i++ {
		if _, err = io.WriteString(w, chunk); err != nil {
			break
		}
	}
	_ = w.Close()
	err = <-closed
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("fragmented budget not enforced: %v", err)
	}
}
