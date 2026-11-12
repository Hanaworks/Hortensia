package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/manager/operations"
)

const testID = "11111111-1111-4111-8111-111111111111"
const testOrigin = "https://manager.example"
const testSecret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// This backend only measures dispatch/rejection; transaction assertions use a
// real Store in integration_test.go.
type boundaryBackend struct {
	calls      int
	authError  error
	loginError error
}

func (b *boundaryBackend) Login(context.Context, string, string) (operations.BrowserLogin, error) {
	b.calls++
	return operations.BrowserLogin{}, b.loginError
}
func (b *boundaryBackend) AuthenticateBrowser(context.Context, string) (*operations.BrowserSession, error) {
	b.calls++
	return nil, b.authError
}
func (b *boundaryBackend) BrowserMe(context.Context, *operations.BrowserSession) (operations.ServiceReply, error) {
	b.calls++
	return operations.ServiceReply{}, errors.New("unexpected")
}
func (b *boundaryBackend) Logout(context.Context, *operations.BrowserSession) error {
	b.calls++
	return nil
}
func (b *boundaryBackend) DoService(context.Context, *operations.BrowserSession, operations.ServiceRequest) (operations.ServiceReply, error) {
	b.calls++
	return operations.ServiceReply{}, errors.New("unexpected")
}
func (b *boundaryBackend) EnrollNode(context.Context, string, operations.EnrollmentRequest) (operations.ServiceReply, error) {
	b.calls++
	return operations.ServiceReply{}, b.loginError
}
func (b *boundaryBackend) RotateNodeCredential(context.Context, string, operations.RotationRequest) (operations.ServiceReply, error) {
	b.calls++
	return operations.ServiceReply{}, b.loginError
}

func request(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:5000"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func errorCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var response struct {
		Error struct {
			Code, Category, Message, RequestID string
			Fields                             []any  `json:"field_errors"`
			Request                            string `json:"request_id"`
		}
	}
	if w.Code != status {
		t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body, status)
	}
	if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	if response.Error.Code != code || response.Error.Request == "" || response.Error.Fields == nil {
		t.Fatalf("invalid error: %s", w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal(w.Header())
	}
}
func TestRejectsStructureBeforeBackend(t *testing.T) {
	cases := []struct {
		name, method, path, body, content string
		status                            int
		code                              string
	}{
		{"unknown", "GET", "/api/v1/unknown", "", "", 404, "not_found"},
		{"method", "PUT", "/api/v1/users", "{}", "application/json", 404, "not_found"},
		{"slash", "GET", "/api/v1//users", "", "", 404, "not_found"},
		{"trailing", "GET", "/api/v1/users/", "", "", 404, "not_found"},
		{"encoded", "GET", "/api/v1/%75sers", "", "", 404, "not_found"},
		{"bad id", "GET", "/api/v1/users/bad", "", "", 400, "invalid_field"},
		{"query duplicate", "GET", "/api/v1/users?limit=2&limit=3", "", "", 400, "invalid_field"},
		{"query unknown", "GET", "/api/v1/users?token=secret", "", "", 400, "invalid_field"},
		{"query percent", "GET", "/api/v1/users?cursor=%ZZ", "", "", 400, "invalid_field"},
		{"list budget", "GET", "/api/v1/users?limit=201", "", "", 400, "invalid_field"},
		{"output budget", "GET", "/api/v1/operations/" + testID + "/output?limit=262145", "", "", 400, "invalid_field"},
		{"output after", "GET", "/api/v1/operations/" + testID + "/output?after=-1", "", "", 400, "invalid_field"},
		{"missing node", "GET", "/api/v1/operations", "", "", 400, "invalid_field"},
		{"read body", "GET", "/api/v1/me", "{}", "application/json", 400, "invalid_field"},
		{"duplicate JSON", "POST", "/api/v1/sessions", `{"username":"admin","username":"other","password":"correct horse battery"}`, "application/json", 400, "invalid_json"},
		{"surrogate", "POST", "/api/v1/sessions", `{"username":"admin","password":"secret-secret\ud800"}`, "application/json", 400, "invalid_json"},
		{"unsafe number", "POST", "/api/v1/sessions", `{"username":"admin","password":9007199254740992}`, "application/json", 400, "invalid_json"},
		{"unknown field", "POST", "/api/v1/sessions", `{"username":"admin","password":"correct horse battery","admin":true}`, "application/json", 400, "invalid_field"},
		{"trailing JSON", "POST", "/api/v1/sessions", "{}{}", "application/json", 400, "invalid_json"},
		{"UTF8", "POST", "/api/v1/sessions", "{\"username\":\"\xff\"}", "application/json", 400, "invalid_json"},
		{"BOM", "POST", "/api/v1/sessions", "\xef\xbb\xbf{}", "application/json", 400, "invalid_json"},
		{"content type", "POST", "/api/v1/sessions", "{}", "text/plain", 400, "invalid_field"},
		{"content param", "POST", "/api/v1/sessions", "{}", "application/json; x=y", 400, "invalid_field"},
		{"payload", "POST", "/api/v1/sessions", strings.Repeat(" ", 8*1024*1024+1), "application/json", 413, "payload_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &boundaryBackend{}
			h, e := New(Config{Store: b, ExternalOrigin: testOrigin})
			if e != nil {
				t.Fatal(e)
			}
			w := request(t, h, tc.method, tc.path, tc.body, map[string]string{"Content-Type": tc.content, "Origin": testOrigin})
			errorCode(t, w, tc.status, tc.code)
			if b.calls != 0 {
				t.Fatal("rejected request reached backend")
			}
		})
	}
}
func TestAuthenticationRateLimitAndSafeErrors(t *testing.T) {
	b := &boundaryBackend{loginError: &operations.ServiceError{Status: 401, Code: "invalid_credentials", Message: "secret SQL details"}}
	h, e := New(Config{Store: b, ExternalOrigin: testOrigin})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 11; i++ {
		w := request(t, h, "POST", "/api/v1/sessions", `{"username":"admin","password":"correct horse battery"}`, map[string]string{"Content-Type": "application/json", "Origin": testOrigin, "X-Forwarded-For": "203.0.113." + string(rune('a'+i))})
		if i < 10 {
			errorCode(t, w, 401, "invalid_credentials")
		} else {
			errorCode(t, w, 429, "rate_limited")
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("missing retry")
			}
		}
		if strings.Contains(w.Body.String(), "secret SQL") {
			t.Fatal("leaked internal error")
		}
	}
	if b.calls != 10 {
		t.Fatal(b.calls)
	}
	b = &boundaryBackend{loginError: errors.New("private SQL password")}
	h, _ = New(Config{Store: b, ExternalOrigin: testOrigin})
	w := request(t, h, "POST", "/api/v1/sessions", `{"username":"admin","password":"correct horse battery"}`, map[string]string{"Content-Type": "application/json", "Origin": testOrigin})
	errorCode(t, w, 500, "internal_error")
	if strings.Contains(w.Body.String(), "private") {
		t.Fatal("leak")
	}
}
func TestLimiterCapacityExpiryAndReservations(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newFailureLimiter(2)
	release, retry := l.acquire(now, []string{"ip", "account"})
	if retry != 0 {
		t.Fatal(retry)
	}
	if _, retry = l.acquire(now, []string{"new"}); retry == 0 {
		t.Fatal("capacity evicted active budget")
	}
	release(true)
	for i := 1; i < 10; i++ {
		finish, retry := l.acquire(now, []string{"ip", "account"})
		if retry != 0 {
			t.Fatal(retry)
		}
		finish(true)
	}
	if _, retry = l.acquire(now, []string{"ip"}); retry != 60 {
		t.Fatal(retry)
	}
	finish, retry := l.acquire(now.Add(time.Minute), []string{"new"})
	if retry != 0 {
		t.Fatal(retry)
	}
	finish(false)
	l = newFailureLimiter(20)
	var held []func(bool)
	for i := 0; i < 10; i++ {
		f, r := l.acquire(now, []string{"ip"})
		if r != 0 {
			t.Fatal(r)
		}
		held = append(held, f)
	}
	if _, r := l.acquire(now, []string{"ip"}); r == 0 {
		t.Fatal("in-flight bypass")
	}
	for _, f := range held {
		f(false)
	}
	if _, r := l.acquire(now, []string{"ip"}); r != 0 {
		t.Fatal("success consumed failure budget")
	}
}

func TestSeparateAccountAndAgentBudgets(t *testing.T) {
	for _, kind := range []string{"login", "agent"} {
		t.Run(kind, func(t *testing.T) {
			b := &boundaryBackend{loginError: &operations.ServiceError{Status: 401, Code: "invalid_credentials"}}
			h, _ := New(Config{Store: b, ExternalOrigin: testOrigin})
			for i := 0; i < 11; i++ {
				path, body := "/api/v1/sessions", `{"username":"admin","password":"correct horse battery"}`
				if kind == "agent" {
					path = "/agent/v1/credentials/rotate"
					body = `{"ledger_id":"` + testID + `","credential_secret":"` + testSecret + `"}`
				}
				r := httptest.NewRequest("POST", path, strings.NewReader(body))
				r.RemoteAddr = "192.0.2." + strconv.Itoa(i+1) + ":1234"
				r.Header.Set("Content-Type", "application/json")
				if kind == "login" {
					r.Header.Set("Origin", testOrigin)
				} else {
					r.Header.Set("Authorization", "Bearer "+testSecret)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if i < 10 {
					errorCode(t, w, 401, "invalid_credentials")
				} else {
					errorCode(t, w, 429, "rate_limited")
				}
			}
			if b.calls != 10 {
				t.Fatal(b.calls)
			}
		})
	}
}
