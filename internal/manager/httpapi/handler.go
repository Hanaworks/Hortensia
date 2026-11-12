// Package httpapi converts the versioned HTTP contract into Store service calls.
// It owns transport validation and ephemeral authentication failure budgets;
// the Store remains responsible for current authorization and atomic changes.
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"docker-manager/internal/manager/operations"
	"docker-manager/internal/protocol"
)

type Backend interface {
	Login(context.Context, string, string) (operations.BrowserLogin, error)
	AuthenticateBrowser(context.Context, string) (*operations.BrowserSession, error)
	BrowserMe(context.Context, *operations.BrowserSession) (operations.ServiceReply, error)
	Logout(context.Context, *operations.BrowserSession) error
	DoService(context.Context, *operations.BrowserSession, operations.ServiceRequest) (operations.ServiceReply, error)
	EnrollNode(context.Context, string, operations.EnrollmentRequest) (operations.ServiceReply, error)
	RotateNodeCredential(context.Context, string, operations.RotationRequest) (operations.ServiceReply, error)
}

var _ Backend = (*operations.Store)(nil)

type Config struct {
	Store          Backend
	ExternalOrigin string
}
type handler struct {
	store   Backend
	origin  string
	limiter *failureLimiter
}

func New(c Config) (http.Handler, error) {
	if c.Store == nil || reflect.ValueOf(c.Store).Kind() == reflect.Ptr && reflect.ValueOf(c.Store).IsNil() {
		return nil, errors.New("HTTP backend is required")
	}
	u, e := url.Parse(c.ExternalOrigin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("external origin must be an HTTPS origin without a path")
	}
	return &handler{store: c.Store, origin: c.ExternalOrigin, limiter: newFailureLimiter(10000)}, nil
}
func validSecret(s string) bool {
	if len(s) != 43 {
		return false
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(s)
	return e == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == s
}
func sessionCookie(r *http.Request) string {
	token := ""
	count := 0
	// net/http silently drops malformed cookies. Count the raw names as well,
	// so a malformed duplicate cannot leave one apparently unique credential.
	for _, header := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(header, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if strings.TrimSpace(name) == "__Host-dm_session" {
				count++
			}
		}
	}
	if count != 1 {
		return ""
	}
	count = 0
	for _, c := range r.Cookies() {
		if c.Name == "__Host-dm_session" {
			count++
			token = c.Value
		}
	}
	if count != 1 || !validSecret(token) {
		return ""
	}
	return token
}
func setCookie(w http.ResponseWriter, token string) {
	c := &http.Cookie{Name: "__Host-dm_session", Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60, Expires: time.Now().UTC().Add(12 * time.Hour)}
	if token == "" {
		c.MaxAge = -1
		c.Expires = time.Unix(1, 0).UTC()
	}
	http.SetCookie(w, c)
}

func readBody(r *http.Request, rt route) (json.RawMessage, map[string]any, string) {
	if r.ContentLength > 8*1024*1024 {
		return nil, nil, "payload_too_large"
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024+1))
	if e != nil {
		return nil, nil, "invalid_json"
	}
	if len(raw) > 8*1024*1024 {
		return nil, nil, "payload_too_large"
	}
	if r.Method == "GET" {
		if len(raw) != 0 {
			return nil, nil, "invalid_field"
		}
		return nil, nil, ""
	}
	if len(raw) == 0 {
		if rt.empty {
			return nil, nil, ""
		}
		return nil, nil, "invalid_json"
	}
	ct, ok := singleHeader(r, "Content-Type")
	if !ok {
		return nil, nil, "invalid_field"
	}
	media, params, e := mime.ParseMediaType(ct)
	if e != nil || media != "application/json" || len(params) > 1 {
		return nil, nil, "invalid_field"
	}
	for k, v := range params {
		if k != "charset" || !strings.EqualFold(v, "utf-8") {
			return nil, nil, "invalid_field"
		}
	}
	if len(r.Header.Values("Content-Encoding")) > 0 {
		return nil, nil, "invalid_field"
	}
	value, e := protocol.StrictJSON(raw)
	if e != nil {
		return nil, nil, "invalid_json"
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, nil, "invalid_field"
	}
	if rt.empty && len(obj) != 0 {
		return nil, nil, "invalid_field"
	}
	return raw, obj, ""
}
func exactStrings(obj map[string]any, keys ...string) ([]string, bool) {
	if len(obj) != len(keys) {
		return nil, false
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		v, ok := obj[k].(string)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}
func writeReply(w http.ResponseWriter, id string, r operations.ServiceReply) {
	if r.Status != 200 && r.Status != 201 && r.Status != 202 && r.Status != 204 {
		writeError(w, id, "internal_error", 0)
		return
	}
	if r.Status != 204 && !json.Valid(r.Body) {
		writeError(w, id, "internal_error", 0)
		return
	}
	if r.ETag != "" {
		w.Header().Set("ETag", r.ETag)
	}
	if r.Location != "" {
		w.Header().Set("Location", r.Location)
	}
	w.WriteHeader(r.Status)
	if r.Status != 204 {
		_, _ = w.Write(r.Body)
	}
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	id, e := protocol.NewID()
	if e != nil {
		http.Error(w, "Request could not be completed.", 500)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	// Context cancellation alone does not interrupt a socket blocked in Body.Read.
	// Keep this deadline local to ordinary API handling, not the server or a
	// future WebSocket handler. Clear it before returning a keepalive connection.
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	_ = controller.SetReadDeadline(deadline)
	defer controller.SetReadDeadline(time.Time{})
	rt, code := classify(r)
	if code != "" {
		w.Header().Set("Connection", "close")
		writeError(w, id, code, 0)
		return
	}
	sr, code := parseRequest(r, rt)
	if code != "" {
		w.Header().Set("Connection", "close")
		writeError(w, id, code, 0)
		return
	}
	raw, obj, code := readBody(r, rt)
	if code != "" {
		// A failed/oversized read may leave unread bytes. Do not let net/http
		// drain them after the per-request deadline has been cleared.
		w.Header().Set("Connection", "close")
		writeError(w, id, code, 0)
		return
	}
	sr.Body = raw
	if rt.kind == "enroll" || rt.kind == "rotate" {
		h.agent(w, r, id, rt, obj)
		return
	}
	if r.Method != "GET" {
		origin, ok := singleHeader(r, "Origin")
		if !ok || origin != h.origin {
			writeError(w, id, "origin_rejected", 0)
			return
		}
	}
	if rt.kind == "login" {
		h.login(w, r, id, obj)
		return
	}
	var session *operations.BrowserSession
	if token := sessionCookie(r); token != "" {
		session, e = h.store.AuthenticateBrowser(ctx, token)
	} else {
		e = &operations.ServiceError{Status: 401, Code: "unauthenticated"}
	}
	if e != nil {
		if rt.kind != "logout" || !authenticationFailure(e) {
			writeServiceError(w, id, e)
			return
		}
		session = nil
	}
	if session == nil && rt.kind != "logout" {
		writeError(w, id, "unauthenticated", 0)
		return
	}
	if r.Method != "GET" && session != nil {
		csrf, ok := singleHeader(r, "X-CSRF-Token")
		if !ok || !session.CheckCSRF(csrf) {
			writeError(w, id, "csrf_rejected", 0)
			return
		}
	}
	if rt.kind == "logout" {
		if session != nil {
			if e = h.store.Logout(ctx, session); e != nil {
				writeServiceError(w, id, e)
				return
			}
		}
		setCookie(w, "")
		w.WriteHeader(204)
		return
	}
	var reply operations.ServiceReply
	if rt.kind == "me" {
		reply, e = h.store.BrowserMe(ctx, session)
	} else {
		reply, e = h.store.DoService(ctx, session, sr)
	}
	if e != nil {
		writeServiceError(w, id, e)
		return
	}
	writeReply(w, id, reply)
}
func (h *handler) login(w http.ResponseWriter, r *http.Request, id string, obj map[string]any) {
	fields, ok := exactStrings(obj, "username", "password")
	if !ok || !usernamePattern.MatchString(fields[0]) || utf8.RuneCountInString(fields[1]) < 12 || utf8.RuneCountInString(fields[1]) > 128 {
		writeError(w, id, "invalid_field", 0)
		return
	}
	finish, retry := h.limiter.acquire(time.Now(), []string{budgetKey("login-ip", peerIP(r.RemoteAddr)), budgetKey("login-account", fields[0])})
	if retry != 0 {
		writeError(w, id, "rate_limited", retry)
		return
	}
	login, e := h.store.Login(r.Context(), fields[0], fields[1])
	finish(authenticationFailure(e))
	if e != nil {
		writeServiceError(w, id, e)
		return
	}
	if !validSecret(login.Token) || login.Session == nil || login.Reply.Status != 200 || !json.Valid(login.Reply.Body) {
		writeError(w, id, "internal_error", 0)
		return
	}
	setCookie(w, login.Token)
	writeReply(w, id, login.Reply)
}
func (h *handler) agent(w http.ResponseWriter, r *http.Request, id string, rt route, obj map[string]any) {
	if len(r.Header.Values("Origin")) != 0 {
		writeError(w, id, "origin_rejected", 0)
		return
	}
	auth, ok := singleHeader(r, "Authorization")
	scheme, token, found := strings.Cut(auth, " ")
	if !ok || !found || !strings.EqualFold(scheme, "Bearer") || !validSecret(token) {
		writeError(w, id, "invalid_credentials", 0)
		return
	}
	var enroll operations.EnrollmentRequest
	var rotate operations.RotationRequest
	if rt.kind == "enroll" {
		f, ok := exactStrings(obj, "node_id", "ledger_id", "credential_secret")
		if !ok || !idPattern.MatchString(f[0]) || !idPattern.MatchString(f[1]) || !validSecret(f[2]) {
			writeError(w, id, "invalid_field", 0)
			return
		}
		enroll = operations.EnrollmentRequest{NodeID: f[0], LedgerID: f[1], CredentialSecret: f[2]}
	} else {
		f, ok := exactStrings(obj, "ledger_id", "credential_secret")
		if !ok || !idPattern.MatchString(f[0]) || !validSecret(f[1]) {
			writeError(w, id, "invalid_field", 0)
			return
		}
		rotate = operations.RotationRequest{LedgerID: f[0], CredentialSecret: f[1]}
	}
	finish, retry := h.limiter.acquire(time.Now(), []string{budgetKey("agent-ip", peerIP(r.RemoteAddr)), budgetKey("agent-token", token)})
	if retry != 0 {
		writeError(w, id, "rate_limited", retry)
		return
	}
	var reply operations.ServiceReply
	var e error
	if rt.kind == "enroll" {
		reply, e = h.store.EnrollNode(r.Context(), token, enroll)
	} else {
		reply, e = h.store.RotateNodeCredential(r.Context(), token, rotate)
	}
	finish(authenticationFailure(e))
	if e != nil {
		writeServiceError(w, id, e)
		return
	}
	writeReply(w, id, reply)
}
