package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/protocol"
)

func serviceConfig() map[string]any {
	return map[string]any{"files": []any{map[string]any{"path": "compose.yaml", "content_b64": base64.StdEncoding.EncodeToString([]byte("services: {}\n"))}}, "compose_files": []string{"compose.yaml"}, "interpolation_env_files": []string{}, "interpolation_env": map[string]string{}, "profiles": []string{}}
}
func serviceNodeWithGrants(t *testing.T, s *Store, l BrowserLogin) string {
	t.Helper()
	node := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/nodes", map[string]any{"display_name": "service-node"}, newServiceID(t), ""))["node_id"].(string)
	me := replyMap(t, serviceCall(t, s, l.Session, "GET", "/api/v1/users/"+l.Session.user, map[string]any{}, "", ""))
	rev := int(me["auth_rev"].(float64))
	serviceCall(t, s, l.Session, "PUT", "/api/v1/nodes/"+node+"/grants/"+l.Session.user, map[string]any{"capabilities": []string{"node.read", "config.read", "config.write", "stack.deploy", "operation.read", "operation.output.read", "operation.cancel"}}, newServiceID(t), fmt.Sprintf(`"%d"`, rev))
	return node
}

func TestServiceConfigCASPermissionReplayAndAtomicFault(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	ctx := context.Background()
	node := serviceNodeWithGrants(t, s, l)
	key := newServiceID(t)
	body := map[string]any{"project_name": "example", "config": serviceConfig()}
	created := serviceCall(t, s, l.Session, "POST", "/api/v1/nodes/"+node+"/stacks", body, key, "")
	m := replyMap(t, created)
	stack, version := m["stack_id"].(string), m["version_id"].(string)
	got := serviceCall(t, s, l.Session, "GET", "/api/v1/stacks/"+stack+"/versions/"+version, map[string]any{}, "", "")
	if !bytes.Contains(got.Body, []byte(base64.StdEncoding.EncodeToString([]byte("services: {}\n")))) {
		t.Fatal("config changed")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		k := newServiceID(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{"config": serviceConfig()})
			_, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/stacks/" + stack + "/versions", IdempotencyKey: k, IfMatch: created.ETag, Body: raw})
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else {
			var se *ServiceError
			if !errors.As(e, &se) || se.Status != 412 {
				t.Fatal(e)
			}
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
	replay := serviceCall(t, s, l.Session, "POST", "/api/v1/nodes/"+node+"/stacks", body, key, "")
	if !bytes.Equal(created.Body, replay.Body) || created.ETag != replay.ETag {
		t.Fatal("mutable replay")
	}
	var versions, receipts int
	s.db.QueryRow(`SELECT count(*) FROM config_versions`).Scan(&versions)
	s.db.QueryRow(`SELECT count(*) FROM idempotency_records`).Scan(&receipts)
	if _, e := s.db.Exec(`CREATE TRIGGER reject_service_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'test failure'); END`); e != nil {
		t.Fatal(e)
	}
	current := serviceCall(t, s, l.Session, "GET", "/api/v1/stacks/"+stack, map[string]any{}, "", "")
	raw, _ := json.Marshal(map[string]any{"config": serviceConfig()})
	_, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/stacks/" + stack + "/versions", Body: raw, IfMatch: current.ETag, IdempotencyKey: newServiceID(t)})
	if e == nil {
		t.Fatal("fault succeeded")
	}
	s.db.Exec(`DROP TRIGGER reject_service_audit`)
	var v2, r2 int
	s.db.QueryRow(`SELECT count(*) FROM config_versions`).Scan(&v2)
	s.db.QueryRow(`SELECT count(*) FROM idempotency_records`).Scan(&r2)
	if v2 != versions || r2 != receipts {
		t.Fatal("partial mutation", v2, r2)
	}
	if _, e = s.db.Exec(`UPDATE keys SET uses=1048576`); e != nil {
		t.Fatal(e)
	}
	again := serviceCall(t, s, l.Session, "POST", "/api/v1/nodes/"+node+"/stacks", body, key, "")
	if !bytes.Equal(again.Body, created.Body) {
		t.Fatal("exhausted replay changed")
	}
}

func TestServiceEnrollmentRotationAndAuthenticatedRecoveryApply(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	ctx := context.Background()
	node := serviceNodeWithGrants(t, s, l)
	key := newServiceID(t)
	path := "/api/v1/nodes/" + node + "/enrollment-tokens"
	enrollment := replyMap(t, serviceCall(t, s, l.Session, "POST", path, map[string]any{}, key, ""))
	token := enrollment["token"].(string)
	replay := replyMap(t, serviceCall(t, s, l.Session, "POST", path, map[string]any{"replace_pending": false}, key, ""))
	if replay["token"] != nil {
		t.Fatal("token replay")
	}
	ap := filepath.Join(t.TempDir(), "agent.db")
	agent, e := ledger.CreateRelease(ctx, ap, node, "service-agent", riAgentKey(), riJournalKey(), ledger.RecoveryOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer agent.Close()
	secret, _ := randomSecret()
	request := EnrollmentRequest{node, agent.Identity().LedgerID, secret}
	binding, e := s.EnrollNode(ctx, token, request)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.EnrollNode(ctx, token, request)
	if e != nil || !bytes.Equal(binding.Body, again.Body) {
		t.Fatal("lost enroll response", e)
	}
	var enrollAudits int
	if e = s.db.QueryRow(`SELECT count(*) FROM audit_events WHERE node_id=? AND action='node.enroll'`, node).Scan(&enrollAudits); e != nil || enrollAudits != 1 {
		t.Fatal("enrollment audit missing or replay duplicated", e, enrollAudits)
	}
	bad := request
	bad.CredentialSecret, _ = randomSecret()
	if _, e = s.EnrollNode(ctx, token, bad); e == nil {
		t.Fatal("changed enrollment accepted")
	}
	a, e := s.AuthenticateNodeCredential(ctx, secret)
	if e != nil {
		t.Fatal(e)
	}
	session := newServiceID(t)
	profile := json.RawMessage(fmt.Sprintf(`{"docker_endpoint_id":%q,"policy_revision":1}`, newServiceID(t)))
	if e = s.BindAuthenticatedNodeSession(ctx, a, session, riReadAgentBoot(t, ap), profile, []string{"compose.apply.v1", "config.bundle.v1"}); e != nil {
		t.Fatal(e)
	}
	created := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/nodes/"+node+"/stacks", map[string]any{"project_name": "accepted", "config": serviceConfig()}, newServiceID(t), ""))
	stack, version := created["stack_id"].(string), created["version_id"].(string)
	raw, _ := json.Marshal(map[string]any{"version_id": version})
	_, e = s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/stacks/" + stack + "/apply", Body: raw, IdempotencyKey: newServiceID(t)})
	var se *ServiceError
	if !errors.As(e, &se) || se.Code != "recovery_required" {
		t.Fatalf("frozen apply: %v", e)
	}
	p := &recoveryPair{m: s, a: agent, b: Binding{node, agent.Identity().LedgerID, session}, agentPath: ap, agentBoot: riReadAgentBoot(t, ap)}
	if e = agent.BindRecoverySessionFixture(ctx, session); e != nil {
		t.Fatal(e)
	}
	riScan(t, p, "all", 10)
	riComplete(t, p)
	accepted := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/stacks/"+stack+"/apply", map[string]any{"version_id": version}, newServiceID(t), ""))
	operation := accepted["operation_id"].(string)
	claims, e := s.Claims(ctx, operation)
	if e != nil || len(claims) != 2 {
		t.Fatal("claims", e)
	}
	op, e := s.Get(ctx, operation)
	if e != nil {
		t.Fatal(e)
	}
	ready := ctlFixture(t, "ready")
	ready["session_id"] = session
	readyBody := ready["body"].(map[string]any)
	readyBody["operation_id"] = operation
	readyBody["input_digest"] = op.Digest
	readyWire, _ := json.Marshal(ready)
	if e = s.ReceiveReady(ctx, a, session, readyWire); e != nil {
		t.Fatal("formal ready", e)
	}
	startWire, e := protocol.NewMessage("request_start", session, "", map[string]any{"operation_id": operation, "input_digest": op.Digest, "start_nonce": newServiceID(t)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuthorizeStart(ctx, p.b, startWire); e != nil {
		t.Fatal(e)
	}
	op, e = s.Get(ctx, operation)
	if e != nil {
		t.Fatal(e)
	}
	cancel := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/operations/"+operation+"/cancel", map[string]any{}, "", ""))
	if cancel["cancel_requested"] != true || cancel["state"] != "authorized" {
		t.Fatal("cancel invented terminal")
	}
	serviceCall(t, s, l.Session, "GET", "/api/v1/operations/"+operation+"/output", map[string]any{}, "", "")
	// The result is a controlled protocol fact, not a claim of real Compose execution.
	result := eventResult(t, s, p.b, op, "normal_exit", 0, "complete", "consistent", "")
	var resultMessage map[string]any
	json.Unmarshal(result, &resultMessage)
	resultMessage["body"].(map[string]any)["output_b64"] = base64.StdEncoding.EncodeToString([]byte("中文output"))
	result, _ = json.Marshal(resultMessage)
	if _, e = s.ReceiveEvent(ctx, p.b, result); e != nil {
		t.Fatal("result", e)
	}
	stackView := replyMap(t, serviceCall(t, s, l.Session, "GET", "/api/v1/stacks/"+stack, map[string]any{}, "", ""))
	if stackView["last_applied_version_id"] != version || stackView["observation"] == nil {
		t.Fatal("result projection missing", stackView)
	}
	operationView := replyMap(t, serviceCall(t, s, l.Session, "GET", "/api/v1/operations/"+operation, map[string]any{}, "", ""))
	if operationView["release_status"] != "pending" || operationView["state"] != "succeeded" {
		t.Fatal("result state", operationView)
	}
	if _, ok := operationView["result"].(map[string]any)["output_b64"]; ok {
		t.Fatal("result leaked output")
	}
	if _, e = s.DoService(ctx, l.Session, ServiceRequest{Method: "GET", Path: "/api/v1/operations/" + operation + "/output", Limit: 1}); e == nil {
		t.Fatal("split UTF8 allowed")
	}
	out, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "GET", Path: "/api/v1/operations/" + operation + "/output", Limit: 3})
	if e != nil || replyMap(t, out)["text"] != "中" || replyMap(t, out)["next_cursor"] != "3" {
		t.Fatal("UTF8 page", e)
	}
	next, _ := randomSecret()
	if _, e = s.RotateNodeCredential(ctx, secret, RotationRequest{request.LedgerID, next}); e != nil {
		t.Fatal(e)
	}
	var rotateAudits int
	if e = s.db.QueryRow(`SELECT count(*) FROM audit_events WHERE node_id=? AND action='credential.rotate'`, node).Scan(&rotateAudits); e != nil || rotateAudits != 1 {
		t.Fatal("rotation audit", e, rotateAudits)
	}
	if _, e = s.AuthenticateNodeCredential(ctx, secret); e == nil {
		t.Fatal("old credential works")
	}
	if _, e = s.AuthenticateNodeCredential(ctx, next); e != nil {
		t.Fatal(e)
	}
	if e = s.RecordNodeSeen(ctx, a, session); e == nil {
		t.Fatal("stale authenticated handle works")
	}
	claims, e = s.Claims(ctx, operation)
	if e != nil || len(claims) != 2 {
		t.Fatal("rotation removed claims", e)
	}
}

func TestServiceStorageFullAndAgentAuditFailureAreAtomic(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	ctx := context.Background()
	node := serviceNodeWithGrants(t, s, l)
	enrollment := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/nodes/"+node+"/enrollment-tokens", map[string]any{}, newServiceID(t), ""))
	token := enrollment["token"].(string)
	secret, _ := randomSecret()
	request := EnrollmentRequest{node, newServiceID(t), secret}
	if _, e := s.db.Exec(`CREATE TRIGGER fail_agent_audit BEFORE INSERT ON audit_events WHEN NEW.node_id IS NOT NULL BEGIN SELECT RAISE(ABORT,'injected'); END`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.EnrollNode(ctx, token, request); e == nil {
		t.Fatal("audit fault enrolled")
	}
	var credentials, generation, consumed int
	s.db.QueryRow(`SELECT count(*) FROM node_credentials`).Scan(&credentials)
	s.db.QueryRow(`SELECT credential_generation FROM nodes WHERE id=?`, node).Scan(&generation)
	s.db.QueryRow(`SELECT count(*) FROM enrollments WHERE consumed_at IS NOT NULL`).Scan(&consumed)
	if credentials != 0 || generation != 0 || consumed != 0 {
		t.Fatal("partial enrollment")
	}
	s.db.Exec(`DROP TRIGGER fail_agent_audit`)
	if _, e := s.EnrollNode(ctx, token, request); e != nil {
		t.Fatal(e)
	}
	var pages int
	if e := s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); e != nil {
		t.Fatal(e)
	}
	config := serviceConfig()
	config["files"] = []any{map[string]any{"path": "compose.yaml", "content_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 512*1024))}}
	raw, _ := json.Marshal(map[string]any{"project_name": "diskfull", "config": config})
	_, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/nodes/" + node + "/stacks", IdempotencyKey: newServiceID(t), Body: raw})
	var se *ServiceError
	if !errors.As(e, &se) || se.Code != "storage_full" {
		t.Fatalf("full mapping: %v", e)
	}
	var stacks, versions int
	s.db.QueryRow(`SELECT count(*) FROM stacks`).Scan(&stacks)
	s.db.QueryRow(`SELECT count(*) FROM config_versions`).Scan(&versions)
	if stacks != 0 || versions != 0 {
		t.Fatal("full published partial config")
	}
}

func TestServicePasswordCostAndBudget(t *testing.T) {
	start := time.Now()
	hash, e := hashPassword(context.Background(), "test password unicode测试")
	if e != nil {
		t.Fatal(e)
	}
	ok, e := checkPassword(context.Background(), "test password unicode测试", hash)
	if !ok || e != nil {
		t.Fatal(e)
	}
	t.Logf("Argon2id create+verify elapsed=%s memory=64MiB iterations=3 parallelism=1", time.Since(start))
	passwordSlots <- struct{}{}
	passwordSlots <- struct{}{}
	defer func() { <-passwordSlots; <-passwordSlots }()
	if _, e = hashPassword(context.Background(), "test password unicode测试"); e == nil {
		t.Fatal("unbounded hash concurrency")
	}
}

func TestServicePagingPermissionsAndCursorBinding(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	ctx := context.Background()
	first := serviceNodeWithGrants(t, s, l)
	serviceNodeWithGrants(t, s, l)
	page, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "GET", Path: "/api/v1/nodes", Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	m := replyMap(t, page)
	cursor := m["next_cursor"].(string)
	next, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "GET", Path: "/api/v1/nodes", Limit: 1, Cursor: cursor})
	if e != nil {
		t.Fatal(e)
	}
	a := m["items"].([]any)[0].(map[string]any)["node_id"]
	b := replyMap(t, next)["items"].([]any)[0].(map[string]any)["node_id"]
	if a == b || replyMap(t, next)["next_cursor"] != nil {
		t.Fatal("pagination repeated or overflow")
	}
	if _, e = s.DoService(ctx, l.Session, ServiceRequest{Method: "GET", Path: "/api/v1/users", Cursor: cursor}); e == nil {
		t.Fatal("cursor crossed route")
	}
	serviceCall(t, s, l.Session, "PUT", "/api/v1/nodes/"+first+"/grants/"+l.Session.user, map[string]any{"capabilities": []string{}}, newServiceID(t), `"2"`)
	if _, e = s.DoService(ctx, l.Session, ServiceRequest{Method: "GET", Path: "/api/v1/nodes", Cursor: cursor}); e == nil {
		t.Fatal("stale auth cursor")
	}
	node := replyMap(t, serviceCall(t, s, l.Session, "GET", "/api/v1/nodes/"+first, map[string]any{}, "", ""))
	if len(node["capabilities"].([]any)) != 0 || node["execution_profile"] != nil || len(node["agent_capabilities"].([]any)) != 0 {
		t.Fatal("admin metadata leaked")
	}
}

func TestServiceFinalTransactionCommitFaultDoesNotPublish(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	ctx := context.Background()
	_, e := s.db.Exec(`CREATE TABLE service_commit_fault(id TEXT REFERENCES users(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER service_commit_trigger AFTER INSERT ON audit_events BEGIN INSERT INTO service_commit_fault(id) VALUES('absent'); END`)
	if e != nil {
		t.Fatal(e)
	}
	key := newServiceID(t)
	_, e = s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/nodes", IdempotencyKey: key, Body: json.RawMessage(`{"display_name":"must not publish"}`)})
	if e == nil {
		t.Fatal("deferred commit unexpectedly succeeded")
	}
	var nodes, receipts, audits int
	s.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&nodes)
	s.db.QueryRow(`SELECT count(*) FROM idempotency_records`).Scan(&receipts)
	s.db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&audits)
	if nodes != 0 || receipts != 0 || audits != 0 {
		t.Fatal("commit published partial mutation", nodes, receipts, audits)
	}
	s.db.Exec(`DROP TRIGGER service_commit_trigger; DROP TABLE service_commit_fault`)
	serviceCall(t, s, l.Session, "POST", "/api/v1/nodes", map[string]any{"display_name": "must not publish"}, key, "")
}

func TestServiceSessionDeadlinesPasswordRevocationAndLastAdmin(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	ctx := context.Background()
	var beforeHash string
	var beforeRev, beforeAudits int
	s.db.QueryRow(`SELECT password_hash,auth_rev FROM users WHERE id=?`, l.Session.user).Scan(&beforeHash, &beforeRev)
	s.db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&beforeAudits)
	_, badKeyErr := s.DoService(ctx, l.Session, ServiceRequest{Method: "PATCH", Path: "/api/v1/users/" + l.Session.user, IdempotencyKey: newServiceID(t), IfMatch: `"0"`, Body: json.RawMessage(`{"enabled password":true}`)})
	if badKeyErr == nil {
		t.Fatal("joined unknown key changed admin credentials")
	}
	if _, e := s.Login(ctx, "admin", "correct horse battery"); e != nil {
		t.Fatal("malformed patch locked admin out", e)
	}
	var afterHash string
	var afterRev, afterAudits int
	s.db.QueryRow(`SELECT password_hash,auth_rev FROM users WHERE id=?`, l.Session.user).Scan(&afterHash, &afterRev)
	s.db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&afterAudits)
	if beforeHash != afterHash || beforeRev != afterRev || beforeAudits != afterAudits {
		t.Fatal("malformed patch changed durable state")
	}
	if _, e := s.AuthenticateBrowser(ctx, l.Token); e != nil {
		t.Fatal("malformed patch revoked session", e)
	}
	_, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "PATCH", Path: "/api/v1/users/" + l.Session.user, IdempotencyKey: newServiceID(t), IfMatch: `"0"`, Body: json.RawMessage(`{"enabled":false}`)})
	var se *ServiceError
	if !errors.As(e, &se) || se.Code != "last_admin" {
		t.Fatal("last admin", e)
	}
	serviceCall(t, s, l.Session, "PATCH", "/api/v1/users/"+l.Session.user, map[string]any{"password": "new admin long password"}, newServiceID(t), `"0"`)
	if _, e = s.AuthenticateBrowser(ctx, l.Token); e == nil {
		t.Fatal("password change kept session")
	}
	fresh, e := s.Login(ctx, "admin", "new admin long password")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`UPDATE user_sessions SET last_seen_at=? WHERE id=?`, time.Now().Add(-30*time.Minute).UnixMilli(), fresh.Session.id); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuthenticateBrowser(ctx, fresh.Token); e == nil {
		t.Fatal("idle deadline inclusive")
	}
	fresh, e = s.Login(ctx, "admin", "new admin long password")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`UPDATE user_sessions SET expires_at=? WHERE id=?`, time.Now().UnixMilli(), fresh.Session.id); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuthenticateBrowser(ctx, fresh.Token); e == nil {
		t.Fatal("absolute deadline inclusive")
	}
}

func TestServiceMalformedCanonicalAndInitializationPreservation(t *testing.T) {
	s, l, keys, path := serviceFixture(t)
	ctx := context.Background()
	_, cancelErr := s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/operations/" + newServiceID(t) + "/cancel"})
	var cancelHTTP *ServiceError
	if !errors.As(cancelErr, &cancelHTTP) || cancelHTTP.Status != 404 {
		t.Fatal("unknown cancel status", cancelErr)
	}
	var before string
	s.db.QueryRow(`SELECT id FROM users`).Scan(&before)
	if _, e := CreateService(ctx, path, s.Identity().ManagerID, keys, Bootstrap{"another", "another long password"}); e == nil {
		t.Fatal("reinit overwrote")
	}
	var after string
	s.db.QueryRow(`SELECT id FROM users`).Scan(&after)
	if before != after {
		t.Fatal("bootstrap changed")
	}
	for _, raw := range []string{`{"display_name":"one","display_name":"two"}`, `{"display_name":"\ud800"}`, `{"display_name":null}`, `{"display_name":"ok","extra":true}`} {
		if _, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/nodes", Body: json.RawMessage(raw), IdempotencyKey: newServiceID(t)}); e == nil {
			t.Fatalf("malformed accepted: %q", raw)
		}
	}
	key := newServiceID(t)
	one := serviceCall(t, s, l.Session, "POST", "/api/v1/nodes", map[string]any{"display_name": "same"}, key, "")
	raw := json.RawMessage(`{"display_name":"\u0073ame"}`)
	two, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/nodes", Body: raw, IdempotencyKey: key})
	if e != nil || !bytes.Equal(one.Body, two.Body) {
		t.Fatal("equivalent escape not replayed", e)
	}
	_, e = s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/nodes", Body: json.RawMessage(`{"display_name":"different"}`), IdempotencyKey: key})
	var se *ServiceError
	if !errors.As(e, &se) || se.Code != "idempotency_conflict" {
		t.Fatal("changed input not conflicted", e)
	}
}

func TestServiceSessionReadFailureIsNotAuthenticationFailure(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	if _, e := s.db.Exec(`ALTER TABLE user_sessions RENAME COLUMN expires_at TO injected_missing_expiry`); e != nil {
		t.Fatal(e)
	}
	_, e := s.BrowserMe(context.Background(), l.Session)
	if !errors.Is(e, ErrStorage) {
		t.Fatalf("session read failure became auth response: %v", e)
	}
}

func serviceFixture(t *testing.T) (*Store, BrowserLogin, Keys, string) {
	t.Helper()
	ctx := context.Background()
	id, _ := protocol.NewID()
	keys := Keys{DigestID: "service-digest", EncryptionID: "service-encryption", DigestKey: make([]byte, 32), EncryptionKey: make([]byte, 32)}
	keys.EncryptionKey[0] = 1
	p := filepath.Join(t.TempDir(), "manager.db")
	s, e := CreateService(ctx, p, id, keys, Bootstrap{"admin", "correct horse battery"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	login, e := s.Login(ctx, "admin", "correct horse battery")
	if e != nil {
		t.Fatal(e)
	}
	return s, login, keys, p
}
func serviceCall(t *testing.T, s *Store, b *BrowserSession, method, path string, body any, key, etag string) ServiceReply {
	t.Helper()
	raw, _ := json.Marshal(body)
	r, e := s.DoService(context.Background(), b, ServiceRequest{Method: method, Path: path, Body: raw, IdempotencyKey: key, IfMatch: etag})
	if e != nil {
		t.Fatalf("%s %s: %v", method, path, e)
	}
	return r
}
func replyMap(t *testing.T, r ServiceReply) map[string]any {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal(r.Body, &m); e != nil {
		t.Fatal(e)
	}
	return m
}
func newServiceID(t *testing.T) string {
	t.Helper()
	id, e := protocol.NewID()
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func TestServiceAuthenticationAndVersionBoundary(t *testing.T) {
	s, l, k, p := serviceFixture(t)
	ctx := context.Background()
	if !l.Session.CheckCSRF(replyMap(t, l.Reply)["csrf_token"].(string)) || l.Session.CheckCSRF("bad") {
		t.Fatal("CSRF")
	}
	if _, e := s.Login(ctx, "admin", "incorrect password"); e == nil {
		t.Fatal("bad password accepted")
	}
	if e := s.CreateUserFixture(ctx, newServiceID(t), true); e != ErrSchema {
		t.Fatalf("fixture: %v", e)
	}
	if _, e := OpenRelease(ctx, p, s.Identity(), k); e != ErrSchema {
		t.Fatalf("v3 opened v4: %v", e)
	}
	s2, e := OpenService(ctx, p, s.Identity(), k)
	if e != nil {
		t.Fatal(e)
	}
	defer s2.Close()
	if _, e = s.AuthenticateBrowser(ctx, l.Token); e == nil {
		t.Fatal("old boot can authenticate")
	}
	if _, e = s2.AuthenticateBrowser(ctx, l.Token); e != nil {
		t.Fatal(e)
	}
}
func TestServiceGrantKeepsSessionAndReplayIsImmutable(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	ctx := context.Background()
	u := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/users", map[string]any{"username": "alice", "password": "alice secret password"}, newServiceID(t), ""))["user_id"].(string)
	alice, e := s.Login(ctx, "alice", "alice secret password")
	if e != nil {
		t.Fatal(e)
	}
	node := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/nodes", map[string]any{"display_name": "node"}, newServiceID(t), ""))["node_id"].(string)
	key := newServiceID(t)
	r := serviceCall(t, s, l.Session, "PUT", "/api/v1/nodes/"+node+"/grants/"+u, map[string]any{"capabilities": []string{"node.read"}}, key, `"0"`)
	if r.ETag != `"1"` {
		t.Fatalf("etag %s", r.ETag)
	}
	if _, e = s.AuthenticateBrowser(ctx, alice.Token); e != nil {
		t.Fatal("grant revoked session", e)
	}
	serviceCall(t, s, l.Session, "PUT", "/api/v1/nodes/"+node+"/grants/"+u, map[string]any{"capabilities": []string{}}, newServiceID(t), `"1"`)
	replay := serviceCall(t, s, l.Session, "PUT", "/api/v1/nodes/"+node+"/grants/"+u, map[string]any{"capabilities": []string{"node.read"}}, key, `"0"`)
	if string(replay.Body) != string(r.Body) || replay.ETag != r.ETag {
		t.Fatal("replay changed")
	}
	if _, e = s.DoService(ctx, alice.Session, ServiceRequest{Method: "GET", Path: "/api/v1/nodes/" + node}); e == nil {
		t.Fatal("removed grant remained")
	}
}
