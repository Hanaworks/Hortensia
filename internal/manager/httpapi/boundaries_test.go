package httpapi

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/manager/operations"
)

func TestAllBrowserRoutesRequireSessionAndRejectExtraQuery(t *testing.T) {
	// These cases enumerate the published HTTP contract independently of routes.
	cases := []struct{ method, path, body, condition string }{
		{"GET", "/api/v1/me", "", ""}, {"POST", "/api/v1/users", `{"username":"alice","password":"alice secret password"}`, ""},
		{"GET", "/api/v1/users", "", ""}, {"GET", "/api/v1/users/" + testID, "", ""}, {"PATCH", "/api/v1/users/" + testID, `{"enabled":true}`, `"0"`},
		{"POST", "/api/v1/nodes", `{"display_name":"node"}`, ""}, {"GET", "/api/v1/nodes", "", ""}, {"GET", "/api/v1/nodes/" + testID, "", ""},
		{"GET", "/api/v1/nodes/" + testID + "/grants", "", ""}, {"PUT", "/api/v1/nodes/" + testID + "/grants/" + testID, `{"capabilities":[]}`, `"0"`},
		{"POST", "/api/v1/nodes/" + testID + "/enrollment-tokens", `{}`, ""}, {"POST", "/api/v1/nodes/" + testID + "/revoke", "", ""},
		{"GET", "/api/v1/nodes/" + testID + "/stacks", "", ""}, {"POST", "/api/v1/nodes/" + testID + "/stacks", `{}`, ""},
		{"GET", "/api/v1/stacks/" + testID, "", ""}, {"GET", "/api/v1/stacks/" + testID + "/versions/" + testID, "", ""},
		{"POST", "/api/v1/stacks/" + testID + "/versions", `{}`, `"` + testID + `"`}, {"POST", "/api/v1/stacks/" + testID + "/apply", `{"version_id":"` + testID + `"}`, ""},
		{"GET", "/api/v1/operations/" + testID, "", ""}, {"GET", "/api/v1/operations?node_id=" + testID, "", ""},
		{"GET", "/api/v1/submissions/" + testID, "", ""}, {"POST", "/api/v1/operations/" + testID + "/cancel", "", ""},
		{"GET", "/api/v1/operations/" + testID + "/output", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			b := &boundaryBackend{}
			h, _ := New(Config{Store: b, ExternalOrigin: testOrigin})
			headers := map[string]string{"Content-Type": "application/json", "Origin": testOrigin, "Idempotency-Key": testID}
			if tc.condition != "" {
				headers["If-Match"] = tc.condition
			}
			w := request(t, h, tc.method, tc.path, tc.body, headers)
			errorCode(t, w, 401, "unauthenticated")
			sep := "?"
			if strings.Contains(tc.path, "?") {
				sep = "&"
			}
			w = request(t, h, tc.method, tc.path+sep+"unexpected=yes", tc.body, headers)
			errorCode(t, w, 400, "invalid_field")
			if b.calls != 0 {
				t.Fatal("backend called without authentication")
			}
		})
	}
}

func TestHeadersAndAgentBoundary(t *testing.T) {
	cases := []struct {
		name, method, path, body string
		headers                  http.Header
		status                   int
		code                     string
	}{
		{"missing key", "POST", "/api/v1/nodes", `{}`, http.Header{}, 400, "invalid_field"},
		{"duplicate key", "POST", "/api/v1/nodes", `{}`, http.Header{"Idempotency-Key": {testID, testID}}, 400, "invalid_field"},
		{"missing condition", "PATCH", "/api/v1/users/" + testID, `{}`, http.Header{"Idempotency-Key": {testID}}, 428, "precondition_required"},
		{"weak condition", "PATCH", "/api/v1/users/" + testID, `{}`, http.Header{"Idempotency-Key": {testID}, "If-Match": {`W/"0"`}}, 400, "invalid_field"},
		{"wildcard condition", "PATCH", "/api/v1/users/" + testID, `{}`, http.Header{"Idempotency-Key": {testID}, "If-Match": {"*"}}, 400, "invalid_field"},
		{"duplicate condition", "PATCH", "/api/v1/users/" + testID, `{}`, http.Header{"Idempotency-Key": {testID}, "If-Match": {`"0"`, `"0"`}}, 400, "invalid_field"},
		{"agent origin", "POST", "/agent/v1/enroll", `{}`, http.Header{"Origin": {testOrigin}, "Authorization": {"Bearer " + testSecret}}, 403, "origin_rejected"},
		{"agent empty origin", "POST", "/agent/v1/credentials/rotate", `{}`, http.Header{"Origin": {""}, "Authorization": {"Bearer " + testSecret}}, 403, "origin_rejected"},
		{"agent cookie", "POST", "/agent/v1/enroll", `{}`, http.Header{"Cookie": {"__Host-dm_session=" + testSecret}}, 401, "invalid_credentials"},
		{"agent duplicate bearer", "POST", "/agent/v1/enroll", `{}`, http.Header{"Authorization": {"Bearer " + testSecret, "Bearer " + testSecret}}, 401, "invalid_credentials"},
		{"agent extra field", "POST", "/agent/v1/enroll", `{"node_id":"` + testID + `","ledger_id":"` + testID + `","credential_secret":"` + testSecret + `","extra":true}`, http.Header{"Authorization": {"Bearer " + testSecret}}, 400, "invalid_field"},
		{"agent missing field", "POST", "/agent/v1/credentials/rotate", `{"ledger_id":"` + testID + `"}`, http.Header{"Authorization": {"Bearer " + testSecret}}, 400, "invalid_field"},
		{"duplicate origin", "POST", "/api/v1/sessions", `{"username":"admin","password":"correct horse battery"}`, http.Header{"Origin": {testOrigin, testOrigin}}, 403, "origin_rejected"},
		{"duplicate cookie", "GET", "/api/v1/me", "", http.Header{"Cookie": {"__Host-dm_session=" + testSecret, "__Host-dm_session=" + testSecret}}, 401, "unauthenticated"},
		{"malformed duplicate cookie", "GET", "/api/v1/me", "", http.Header{"Cookie": {"__Host-dm_session=" + testSecret + "; __Host-dm_session=bad\x01"}}, 401, "unauthenticated"},
		{"spaced malformed duplicate cookie", "GET", "/api/v1/me", "", http.Header{"Cookie": {"__Host-dm_session=" + testSecret + "; __Host-dm_session =bad\x01"}}, 401, "unauthenticated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &boundaryBackend{}
			h, _ := New(Config{Store: b, ExternalOrigin: testOrigin})
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header = tc.headers
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			errorCode(t, w, tc.status, tc.code)
			if b.calls != 0 {
				t.Fatal("invalid input reached backend")
			}
		})
	}
}

func TestQueryBoundsAndCanonicalForwarding(t *testing.T) {
	for _, tc := range []struct {
		path  string
		limit int
		after string
	}{
		{"/api/v1/users", 0, ""}, {"/api/v1/users?limit=200&cursor=abc", 200, ""},
		{"/api/v1/operations/" + testID + "/output?limit=262144&after=12", 262144, "12"},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		rt, code := classify(r)
		if code != "" {
			t.Fatal(code)
		}
		sr, code := parseRequest(r, rt)
		if code != "" || sr.Limit != tc.limit || sr.After != tc.after || sr.Path != r.URL.Path {
			t.Fatal(sr, code)
		}
	}
	for _, path := range []string{"/api/v1/users?limit=0", "/api/v1/users?limit=1.0", "/api/v1/users?cursor=", "/api/v1/operations/" + testID + "/output?after=18446744073709551616", "/api/v1/operations/" + testID + "/output?cursor=abc"} {
		r := httptest.NewRequest("GET", path, nil)
		rt, _ := classify(r)
		if _, code := parseRequest(r, rt); code != "invalid_field" {
			t.Fatal(path, code)
		}
	}
}

type deadlineBackend struct {
	boundaryBackend
	deadline time.Time
	started  chan struct{}
}

func (b *deadlineBackend) Login(ctx context.Context, _, _ string) (operations.BrowserLogin, error) {
	b.deadline, _ = ctx.Deadline()
	close(b.started)
	<-ctx.Done()
	return operations.BrowserLogin{}, ctx.Err()
}
func TestDeadlineAndClientCancellation(t *testing.T) {
	b := &deadlineBackend{started: make(chan struct{})}
	h, _ := New(Config{Store: b, ExternalOrigin: testOrigin})
	r := httptest.NewRequest("POST", "/api/v1/sessions", strings.NewReader(`{"username":"admin","password":"correct horse battery"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", testOrigin)
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	start := time.Now()
	go func() { h.ServeHTTP(w, r); close(done) }()
	<-b.started
	if d := b.deadline.Sub(start); d < 29*time.Second || d > 31*time.Second {
		t.Fatal("deadline", d)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client cancellation not propagated")
	}
	errorCode(t, w, 500, "internal_error")
}

func TestDuplicateCSRFCannotMutateRealStore(t *testing.T) {
	f := newTLSFixture(t)
	r := httptest.NewRequest("POST", "/api/v1/nodes", strings.NewReader(`{"display_name":"must not exist"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", f.server.URL)
	r.Header.Set("Idempotency-Key", testID)
	r.Header["X-Csrf-Token"] = []string{f.csrf, f.csrf}
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(w, r)
	errorCode(t, w, 403, "csrf_rejected")
	m, _, _ := f.call("GET", "/api/v1/nodes", nil, 200, nil)
	if len(m["items"].([]any)) != 0 {
		t.Fatal("CSRF failure mutated state")
	}
}

func TestTLSBodyReadHonorsDeadline(t *testing.T) {
	b := &boundaryBackend{}
	server := httptest.NewUnstartedServer(nil)
	origin := "https://" + server.Listener.Addr().String()
	h, e := New(Config{Store: b, ExternalOrigin: origin})
	if e != nil {
		t.Fatal(e)
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
	server.StartTLS()
	defer server.Close()
	conn, e := tls.Dial("tcp", server.Listener.Addr().String(), server.Client().Transport.(*http.Transport).TLSClientConfig)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, e = io.WriteString(conn, "POST /api/v1/sessions HTTP/1.1\r\nHost: "+server.Listener.Addr().String()+"\r\nContent-Type: application/json\r\nContent-Length: 100\r\nOrigin: "+origin+"\r\n\r\n{")
	if e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	response, e := http.ReadResponse(bufio.NewReader(conn), nil)
	elapsed := time.Since(start)
	if e != nil {
		t.Fatalf("slow body was not stopped by server deadline: %v", e)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if elapsed > time.Second || response.StatusCode != 400 || b.calls != 0 {
		t.Fatalf("deadline result elapsed=%s status=%d backend=%d", elapsed, response.StatusCode, b.calls)
	}
}

func TestTLSReadDeadlineIsClearedForKeepalive(t *testing.T) {
	b := &boundaryBackend{}
	server := httptest.NewUnstartedServer(nil)
	h, e := New(Config{Store: b, ExternalOrigin: "https://" + server.Listener.Addr().String()})
	if e != nil {
		t.Fatal(e)
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
	server.StartTLS()
	defer server.Close()
	conn, e := tls.Dial("tcp", server.Listener.Addr().String(), server.Client().Transport.(*http.Transport).TLSClientConfig)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	for i := 0; i < 2; i++ {
		if i != 0 {
			<-time.After(150 * time.Millisecond)
		}
		_, e = io.WriteString(conn, "GET /api/v1/me HTTP/1.1\r\nHost: "+server.Listener.Addr().String()+"\r\n\r\n")
		if e != nil {
			t.Fatal(e)
		}
		response, e := http.ReadResponse(reader, nil)
		if e != nil {
			t.Fatal("stale read deadline closed keepalive", e)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 401 || response.Close {
			t.Fatal(response.StatusCode, response.Close)
		}
	}
}
