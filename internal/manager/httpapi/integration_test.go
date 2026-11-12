package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/manager/operations"
	"docker-manager/internal/protocol"
)

type tlsFixture struct {
	t          *testing.T
	store      *operations.Store
	server     *httptest.Server
	cookie     *http.Cookie
	csrf, user string
}

// The Agent has no public hello/boot accessor yet. Read only its actual boot
// metadata for this controlled handshake; no SQL writes or barrier shortcuts.
func readyAgent(t *testing.T, f *tlsFixture, node string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	a, e := ledger.CreateRelease(ctx, path, node, "http-agent", bytes.Repeat([]byte{3}, 32), ledger.JournalKey{ID: "http-journal", Key: bytes.Repeat([]byte{4}, 32)}, ledger.RecoveryOptions{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close() })
	db, e := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if e != nil {
		t.Fatal(e)
	}
	var boot string
	e = db.QueryRow("SELECT active_boot FROM recovery_control WHERE id=1").Scan(&boot)
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	m, _, _ := f.call("POST", "/api/v1/nodes/"+node+"/enrollment-tokens", map[string]any{}, 201, nil)
	f.call("POST", "/agent/v1/enroll", map[string]any{"node_id": node, "ledger_id": a.Identity().LedgerID, "credential_secret": testSecret}, 200, map[string]string{"Authorization": "Bearer " + m["token"].(string)})
	auth, e := f.store.AuthenticateNodeCredential(ctx, testSecret)
	if e != nil {
		t.Fatal(e)
	}
	session := newID(t)
	profile, _ := json.Marshal(map[string]any{"docker_endpoint_id": newID(t), "policy_revision": 1})
	if e = f.store.BindAuthenticatedNodeSession(ctx, auth, session, boot, profile, []string{"compose.apply.v1", "config.bundle.v1"}); e != nil {
		t.Fatal(e)
	}
	if e = a.BindRecoverySessionFixture(ctx, session); e != nil {
		t.Fatal(e)
	}
	binding := operations.Binding{NodeID: node, LedgerID: a.Identity().LedgerID, SessionID: session}
	request, e := f.store.StartRecovery(ctx, binding, "all", 10)
	if e != nil {
		t.Fatal(e)
	}
	for {
		response, e := a.Reconcile(ctx, request)
		if e != nil {
			t.Fatal(e)
		}
		state, e := f.store.ReceiveRecoveryPage(ctx, binding, response)
		if e != nil {
			t.Fatal(e)
		}
		if state.NextCursor == "" {
			break
		}
		request, e = f.store.NextRecoveryRequest(ctx, binding)
		if e != nil {
			t.Fatal(e)
		}
	}
	state, e := f.store.ProcessRecovery(ctx, binding)
	if e != nil || state.Status != "ready" {
		t.Fatal(state, e)
	}
	request, e = f.store.PrepareRecoveryComplete(ctx, binding)
	if e != nil {
		t.Fatal(e)
	}
	response, e := a.CompleteReconcile(ctx, request)
	if e != nil {
		t.Fatal(e)
	}
	state, e = f.store.ReceiveRecoveryCompleted(ctx, binding, response)
	if e != nil || state.Status != "completed" {
		t.Fatal(state, e)
	}
}

func TestTLSRealOperationAcceptedQueryCancelOutputAndPermission(t *testing.T) {
	f := newTLSFixture(t)
	n, _, _ := f.call("POST", "/api/v1/nodes", map[string]any{"display_name": "online-node"}, 201, nil)
	node := n["node_id"].(string)
	nodePath := "/api/v1/nodes/" + node
	caps := []string{"node.read", "config.read", "config.write", "stack.deploy", "operation.read", "operation.output.read", "operation.cancel"}
	f.call("PUT", nodePath+"/grants/"+f.user, map[string]any{"capabilities": caps}, 200, map[string]string{"If-Match": `"0"`})
	readyAgent(t, f, node)
	c, _, _ := f.call("POST", nodePath+"/stacks", map[string]any{"project_name": "accepted", "config": configBody()}, 201, nil)
	stack, version := c["stack_id"].(string), c["version_id"].(string)
	key := newID(t)
	accepted, headers, raw := f.call("POST", "/api/v1/stacks/"+stack+"/apply", map[string]any{"version_id": version}, 202, map[string]string{"Idempotency-Key": key})
	op := accepted["operation_id"].(string)
	opPath := "/api/v1/operations/" + op
	if headers.Get("Location") != opPath || accepted["state"] != "accepted" {
		t.Fatal(accepted, headers)
	}
	_, _, replay := f.call("POST", "/api/v1/stacks/"+stack+"/apply", map[string]any{"version_id": version}, 202, map[string]string{"Idempotency-Key": key})
	if !bytes.Equal(raw, replay) {
		t.Fatal("apply replay changed")
	}
	operation, _, _ := f.call("GET", opPath, nil, 200, nil)
	if operation["result"] != nil || operation["release_status"] != "held" || operation["version_id"] != version {
		t.Fatal(operation)
	}
	for _, forbidden := range []string{"input_digest", "event_seq", "phase", "session_id", "permit_id", "user_id"} {
		if _, ok := operation[forbidden]; ok {
			t.Fatal("internal field", forbidden)
		}
	}
	list, _, _ := f.call("GET", "/api/v1/operations?node_id="+node, nil, 200, nil)
	if len(list["items"].([]any)) != 1 {
		t.Fatal(list)
	}
	output, _, _ := f.call("GET", opPath+"/output?after=0&limit=262144", nil, 200, nil)
	if output["text"] != "" || output["next_cursor"] != "0" || output["truncated"] != false {
		t.Fatal(output)
	}
	cancel, _, _ := f.call("POST", opPath+"/cancel", nil, 200, nil)
	if cancel["state"] != "accepted" || cancel["cancel_requested"] != true || cancel["result"] != nil {
		t.Fatal("cancel fabricated execution", cancel)
	}
	f.call("POST", opPath+"/cancel", map[string]any{}, 200, nil)
	f.call("PUT", nodePath+"/grants/"+f.user, map[string]any{"capabilities": []string{"node.read"}}, 200, map[string]string{"If-Match": `"1"`})
	f.call("GET", opPath, nil, 403, nil)
	f.call("GET", opPath+"/output", nil, 403, nil)
	f.call("POST", opPath+"/cancel", nil, 403, nil)
	f.call("PUT", nodePath+"/grants/"+f.user, map[string]any{"capabilities": []string{}}, 200, map[string]string{"If-Match": `"2"`})
	f.call("GET", opPath, nil, 404, nil)
	f.call("GET", opPath+"/output", nil, 404, nil)
}

func newID(t *testing.T) string {
	t.Helper()
	v, e := protocol.NewID()
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func newTLSFixture(t *testing.T) *tlsFixture {
	t.Helper()
	keys := operations.Keys{DigestID: "http-digest", EncryptionID: "http-encryption", DigestKey: bytes.Repeat([]byte{1}, 32), EncryptionKey: bytes.Repeat([]byte{2}, 32)}
	s, e := operations.CreateService(context.Background(), filepath.Join(t.TempDir(), "manager.db"), newID(t), keys, operations.Bootstrap{Username: "admin", Password: "correct horse battery"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	server := httptest.NewUnstartedServer(nil)
	origin := "https://" + server.Listener.Addr().String()
	h, e := New(Config{Store: s, ExternalOrigin: origin})
	if e != nil {
		t.Fatal(e)
	}
	server.Config.Handler = h
	server.StartTLS()
	t.Cleanup(server.Close)
	f := &tlsFixture{t: t, store: s, server: server}
	f.login("admin", "correct horse battery")
	return f
}
func (f *tlsFixture) call(method, path string, body any, status int, headers map[string]string) (map[string]any, http.Header, []byte) {
	f.t.Helper()
	var raw []byte
	if body != nil {
		var e error
		raw, e = json.Marshal(body)
		if e != nil {
			f.t.Fatal(e)
		}
	}
	r, e := http.NewRequest(method, f.server.URL+path, bytes.NewReader(raw))
	if e != nil {
		f.t.Fatal(e)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if !strings.HasPrefix(path, "/agent/") {
		if f.cookie != nil {
			r.AddCookie(f.cookie)
		}
		if method != "GET" {
			r.Header.Set("Origin", f.server.URL)
			r.Header.Set("X-CSRF-Token", f.csrf)
			r.Header.Set("Idempotency-Key", newID(f.t))
		}
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	response, e := f.server.Client().Do(r)
	if e != nil {
		f.t.Fatal(e)
	}
	defer response.Body.Close()
	data, e := io.ReadAll(response.Body)
	if e != nil {
		f.t.Fatal(e)
	}
	if response.StatusCode != status {
		f.t.Fatalf("%s %s status=%d want=%d body=%s", method, path, response.StatusCode, status, data)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		f.t.Fatal("response headers", response.Header)
	}
	var result map[string]any
	if status != 204 {
		if e = json.Unmarshal(data, &result); e != nil {
			f.t.Fatal(e)
		}
	} else if len(data) != 0 {
		f.t.Fatal("204 body")
	}
	return result, response.Header, data
}
func (f *tlsFixture) login(username, password string) {
	m, h, _ := f.call("POST", "/api/v1/sessions", map[string]any{"username": username, "password": password}, 200, nil)
	response := &http.Response{Header: h}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		f.t.Fatal("cookies", cookies)
	}
	c := cookies[0]
	if c.Name != "__Host-dm_session" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 43200 || !validSecret(c.Value) {
		f.t.Fatal("unsafe cookie", c)
	}
	f.cookie = c
	f.csrf = m["csrf_token"].(string)
	f.user = m["user_id"].(string)
}
func configBody() map[string]any {
	return map[string]any{"files": []any{map[string]any{"path": "compose.yaml", "content_b64": "c2VydmljZXM6IHt9Cg=="}}, "compose_files": []string{"compose.yaml"}, "interpolation_env_files": []string{}, "interpolation_env": map[string]string{}, "profiles": []string{}}
}
func assertCode(t *testing.T, m map[string]any, code string) {
	t.Helper()
	if m["error"].(map[string]any)["code"] != code {
		t.Fatal(m)
	}
}

func TestTLSRealStoreLoginEnrollmentConfigCASAndRevocation(t *testing.T) {
	f := newTLSFixture(t)
	me, h, _ := f.call("GET", "/api/v1/me", nil, 200, nil)
	if me["csrf_token"] != f.csrf || h.Get("ETag") != `"0"` {
		t.Fatal(me, h)
	}
	f.call("POST", "/api/v1/nodes", map[string]any{"display_name": "blocked"}, 403, map[string]string{"Origin": "https://evil.example"})
	f.call("POST", "/api/v1/nodes", map[string]any{"display_name": "blocked"}, 403, map[string]string{"X-CSRF-Token": "wrong"})
	users, _, _ := f.call("GET", "/api/v1/users", nil, 200, nil)
	if len(users["items"].([]any)) != 1 {
		t.Fatal("rejected request changed users")
	}
	u, _, _ := f.call("POST", "/api/v1/users", map[string]any{"username": "alice", "password": "alice secret password"}, 201, nil)
	aliceID := u["user_id"].(string)
	f.call("GET", "/api/v1/users/"+aliceID, nil, 200, nil)
	f.call("PATCH", "/api/v1/users/"+aliceID, map[string]any{"enabled": true}, 200, map[string]string{"If-Match": `"0"`})
	node, _, _ := f.call("POST", "/api/v1/nodes", map[string]any{"display_name": "http-node"}, 201, nil)
	nodeID := node["node_id"].(string)
	nodePath := "/api/v1/nodes/" + nodeID
	f.call("GET", "/api/v1/nodes?limit=1", nil, 200, nil)
	metadata, _, _ := f.call("GET", nodePath, nil, 200, nil)
	if len(metadata["capabilities"].([]any)) != 0 || metadata["execution_profile"] != nil || metadata["observation"] != nil {
		t.Fatal("admin metadata exposed capabilities", metadata)
	}
	f.call("GET", nodePath+"/grants", nil, 200, nil)
	key := newID(t)
	enrollment, _, _ := f.call("POST", nodePath+"/enrollment-tokens", map[string]any{}, 201, map[string]string{"Idempotency-Key": key})
	token := enrollment["token"].(string)
	replay, _, _ := f.call("POST", nodePath+"/enrollment-tokens", map[string]any{"replace_pending": false}, 201, map[string]string{"Idempotency-Key": key})
	if replay["token"] != nil || replay["enrollment_id"] != enrollment["enrollment_id"] {
		t.Fatal("secret replay", replay)
	}
	ledgerID := newID(t)
	enroll := map[string]any{"node_id": nodeID, "ledger_id": ledgerID, "credential_secret": testSecret}
	bearer := map[string]string{"Authorization": "Bearer " + token}
	_, _, one := f.call("POST", "/agent/v1/enroll", enroll, 200, bearer)
	_, _, two := f.call("POST", "/agent/v1/enroll", enroll, 200, bearer)
	if !bytes.Equal(one, two) || bytes.Contains(one, []byte(testSecret)) || bytes.Contains(one, []byte(token)) {
		t.Fatal("enroll response", string(one), string(two))
	}
	newSecret := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	f.call("POST", "/agent/v1/credentials/rotate", map[string]any{"ledger_id": ledgerID, "credential_secret": newSecret}, 200, map[string]string{"Authorization": "Bearer " + testSecret})
	f.call("POST", "/agent/v1/credentials/rotate", map[string]any{"ledger_id": ledgerID, "credential_secret": newSecret}, 401, map[string]string{"Authorization": "Bearer " + testSecret})
	f.call("POST", nodePath+"/stacks", map[string]any{"project_name": "denied", "config": configBody()}, 404, nil)
	caps := []string{"node.read", "config.read", "config.write", "stack.deploy", "operation.read", "operation.output.read", "operation.cancel"}
	_, grantHeaders, _ := f.call("PUT", nodePath+"/grants/"+f.user, map[string]any{"capabilities": caps}, 200, map[string]string{"If-Match": `"0"`})
	if grantHeaders.Get("ETag") != `"1"` {
		t.Fatal(grantHeaders)
	}
	stackBody := map[string]any{"project_name": "example", "config": configBody()}
	key = newID(t)
	created, createdHeaders, createdRaw := f.call("POST", nodePath+"/stacks", stackBody, 201, map[string]string{"Idempotency-Key": key})
	stack, version := created["stack_id"].(string), created["version_id"].(string)
	stackPath := "/api/v1/stacks/" + stack
	f.call("GET", nodePath+"/stacks?limit=200", nil, 200, nil)
	f.call("GET", stackPath, nil, 200, nil)
	got, versionHeaders, _ := f.call("GET", stackPath+"/versions/"+version, nil, 200, nil)
	if versionHeaders.Get("ETag") != createdHeaders.Get("ETag") || got["version_id"] != version {
		t.Fatal(got, versionHeaders)
	}
	_, _, replayRaw := f.call("POST", nodePath+"/stacks", stackBody, 201, map[string]string{"Idempotency-Key": key})
	if !bytes.Equal(createdRaw, replayRaw) {
		t.Fatal("mutable replay")
	}
	receipt, _, _ := f.call("GET", "/api/v1/submissions/"+key, nil, 200, nil)
	if receipt["result_id"] != stack {
		t.Fatal(receipt)
	}
	newVersion, newHeaders, _ := f.call("POST", stackPath+"/versions", map[string]any{"config": configBody()}, 201, map[string]string{"If-Match": createdHeaders.Get("ETag")})
	if newVersion["version_id"] == version || newHeaders.Get("ETag") == createdHeaders.Get("ETag") {
		t.Fatal("not immutable")
	}
	conflict, _, _ := f.call("POST", stackPath+"/versions", map[string]any{"config": configBody()}, 412, map[string]string{"If-Match": createdHeaders.Get("ETag")})
	assertCode(t, conflict, "version_conflict")
	offline, _, _ := f.call("POST", stackPath+"/apply", map[string]any{"version_id": version}, 503, nil)
	assertCode(t, offline, "node_unavailable")
	list, _, _ := f.call("GET", "/api/v1/operations?node_id="+nodeID, nil, 200, nil)
	if len(list["items"].([]any)) != 0 {
		t.Fatal("offline apply queued")
	}
	// A separate browser keeps its existing session when its grants change.
	adminCookie, adminCSRF, adminID := f.cookie, f.csrf, f.user
	f.call("PUT", nodePath+"/grants/"+aliceID, map[string]any{"capabilities": caps}, 200, map[string]string{"If-Match": `"1"`})
	f.login("alice", "alice secret password")
	aliceCookie, aliceCSRF := f.cookie, f.csrf
	f.call("GET", stackPath+"/versions/"+version, nil, 200, nil)
	f.call("GET", "/api/v1/users", nil, 403, nil)
	f.cookie, f.csrf, f.user = adminCookie, adminCSRF, adminID
	f.call("PUT", nodePath+"/grants/"+aliceID, map[string]any{"capabilities": []string{}}, 200, map[string]string{"If-Match": `"2"`})
	f.cookie, f.csrf = aliceCookie, aliceCSRF
	f.call("GET", "/api/v1/me", nil, 200, nil)
	for _, path := range []string{nodePath, stackPath, stackPath + "/versions/" + version, "/api/v1/operations?node_id=" + nodeID} {
		m, _, raw := f.call("GET", path, nil, 404, nil)
		assertCode(t, m, "not_found")
		if bytes.Contains(raw, []byte("c2Vydmlj")) {
			t.Fatal("revoked content leak")
		}
	}
	f.cookie, f.csrf = adminCookie, adminCSRF
	f.call("POST", nodePath+"/revoke", nil, 200, nil)
	f.call("DELETE", "/api/v1/sessions/current", nil, 204, nil)
	f.call("GET", "/api/v1/me", nil, 401, nil)
	_, headers, _ := f.call("DELETE", "/api/v1/sessions/current", nil, 204, nil)
	if !strings.Contains(headers.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatal("logout did not clear cookie")
	}
}
