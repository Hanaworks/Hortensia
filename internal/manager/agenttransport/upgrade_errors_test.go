package agenttransport

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTLSAuthenticatedInvalidUpgradeHasFixedJSONError(t *testing.T) {
	f := newTransportPair(t)
	for _, test := range []struct {
		name    string
		headers http.Header
	}{
		{name: "missing upgrade"},
		{name: "untrusted connection", headers: http.Header{"Connection": {"private_connection_marker"}}},
		{name: "untrusted version", headers: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-WebSocket-Version": {"private_version_marker"}}},
		{name: "untrusted key", headers: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-WebSocket-Version": {"13"}, "Sec-WebSocket-Key": {"private_key_marker"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _ := http.NewRequestWithContext(f.ctx, "GET", f.server.URL+"/agent/v1/connect", nil)
			request.Header = test.headers.Clone()
			if request.Header == nil {
				request.Header = http.Header{}
			}
			request.Header.Set("Authorization", "Bearer "+f.secret)
			response, err := f.server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(response.Body, 4096))
			if err != nil {
				t.Fatal(err)
			}
			assertFixedUpgradeError(t, response.StatusCode, response.Header, raw, "invalid_field", 400)
			if !response.Close {
				t.Error("upgrade rejection retained HTTP connection")
			}
			for _, secret := range []string{f.secret, "private_connection_marker", "private_version_marker", "private_key_marker", "WebSocket protocol violation"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Error("upgrade error reflected private input or library diagnostic")
				}
			}
			for _, header := range []string{"Upgrade", "Sec-WebSocket-Version", "Sec-WebSocket-Accept"} {
				if response.Header.Get(header) != "" {
					t.Errorf("rejection leaked upgrade header %s", header)
				}
			}
		})
	}
	// The failed upgrades never create a binding. The same real credential can
	// still complete a valid TLS handshake and current Store recovery afterward.
	p, b := f.dial(t)
	f.recover(t, p, b)
}

func assertFixedUpgradeError(t *testing.T, status int, header http.Header, raw []byte, code string, wantStatus int) {
	t.Helper()
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
			Category  string `json:"category"`
			Retry     string `json:"retry_action"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || status != wantStatus || header.Get("Content-Type") != "application/json" || body.Error.Code != code || body.Error.RequestID == "" {
		t.Fatalf("upgrade rejection bypassed fixed error: status=%d type=%q body=%q parse=%v", status, header.Get("Content-Type"), raw, err)
	}
	if header.Get("Cache-Control") != "no-store" || header.Get("X-Content-Type-Options") != "nosniff" || body.Error.Category == "" || body.Error.Retry == "" {
		t.Fatal("fixed upgrade error missing headers or error fields")
	}
}

func authenticatedUpgradeRequest(t *testing.T, f *transportPair, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(f.ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.secret)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return request
}

func TestUpgradeResponsePreservesMissingHijackerFailure(t *testing.T) {
	f := newTransportPair(t)
	request := authenticatedUpgradeRequest(t, f, "https://localhost/agent/v1/connect")
	request.TLS = &tls.ConnectionState{}
	response := httptest.NewRecorder()
	f.h.ServeHTTP(response, request)
	assertFixedUpgradeError(t, response.Code, response.Header(), response.Body.Bytes(), "internal_error", 500)
	if response.Header().Get("Connection") != "close" {
		t.Fatal("server capability failure retained connection")
	}
}

type failedUpgradeHijacker struct{ http.ResponseWriter }

func (w failedUpgradeHijacker) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w failedUpgradeHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("private_hijack_failure")
}

func TestUpgradeHijackFailureAfter101WritesNoHTTPError(t *testing.T) {
	f := newTransportPair(t)
	request := authenticatedUpgradeRequest(t, f, "https://localhost/agent/v1/connect")
	request.TLS = &tls.ConnectionState{}
	response := httptest.NewRecorder()
	f.h.ServeHTTP(failedUpgradeHijacker{response}, request)
	if response.Code != 101 || response.Body.Len() != 0 {
		t.Fatalf("post-101 error altered committed response: status=%d body=%q", response.Code, response.Body.Bytes())
	}
	if response.Header().Get("Content-Type") != "" {
		t.Fatal("post-101 error changed committed headers")
	}
}

func TestUpgradeHijackFailureAfter101ClosesUntransferredTLSConnection(t *testing.T) {
	f := newTransportPair(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.h.ServeHTTP(failedUpgradeHijacker{w}, r) }))
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Do(authenticatedUpgradeRequest(t, f, server.URL+"/agent/v1/connect"))
	if err == nil {
		response.Body.Close()
		t.Fatal("failed hijack exposed a live upgraded response")
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		t.Fatal("failed hijack did not close the HTTP connection")
	}
}
