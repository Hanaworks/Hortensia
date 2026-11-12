package operations

import (
	"bytes"
	"context"
	"docker-manager/internal/agent/ledger"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestReviewM1ConcurrentSameKeyAndReadOnlyReplay(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	node := serviceNodeWithGrants(t, s, l)
	ctx := context.Background()
	key := newServiceID(t)
	path := "/api/v1/nodes/" + node + "/stacks"
	body, _ := json.Marshal(map[string]any{"project_name": "concurrent_review", "config": serviceConfig()})
	req := ServiceRequest{Method: "POST", Path: path, Body: body, IdempotencyKey: key}
	var wg sync.WaitGroup
	replies := make(chan ServiceReply, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := s.DoService(ctx, l.Session, req); replies <- r; errs <- e }()
	}
	wg.Wait()
	close(replies)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first ServiceReply
	for r := range replies {
		if first.Body == nil {
			first = r
		}
		if !bytes.Equal(first.Body, r.Body) || first.ETag != r.ETag {
			t.Fatal("same key changed result")
		}
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM config_versions`).Scan(&n)
	if n != 1 {
		t.Fatal("duplicate versions", n)
	}
	stack := replyMap(t, first)["stack_id"].(string)
	vbody := map[string]any{"config": serviceConfig()}
	vkey := newServiceID(t)
	vp := "/api/v1/stacks/" + stack + "/versions"
	saved := serviceCall(t, s, l.Session, "POST", vp, vbody, vkey, first.ETag)
	serviceCall(t, s, l.Session, "PUT", "/api/v1/nodes/"+node+"/grants/"+l.Session.user, map[string]any{"capabilities": []string{"node.read", "config.read"}}, newServiceID(t), `"1"`)
	replay := serviceCall(t, s, l.Session, "POST", vp, vbody, vkey, first.ETag)
	if !bytes.Equal(saved.Body, replay.Body) || saved.ETag != replay.ETag {
		t.Fatal("read-only replay changed")
	}
	serviceCall(t, s, l.Session, "PUT", "/api/v1/nodes/"+node+"/grants/"+l.Session.user, map[string]any{"capabilities": []string{}}, newServiceID(t), `"2"`)
	raw, _ := json.Marshal(vbody)
	_, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: vp, Body: raw, IdempotencyKey: vkey, IfMatch: first.ETag})
	var se *ServiceError
	if !errors.As(e, &se) || se.Status != 404 {
		t.Fatalf("replay without visibility: %v", e)
	}
}

func TestReviewM1CipherBindingRejectsVersionSwap(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	node := serviceNodeWithGrants(t, s, l)
	initial := serviceCall(t, s, l.Session, "POST", "/api/v1/nodes/"+node+"/stacks", map[string]any{"project_name": "cipher_review", "config": serviceConfig()}, newServiceID(t), "")
	m := replyMap(t, initial)
	stack, old := m["stack_id"].(string), m["version_id"].(string)
	saved := serviceCall(t, s, l.Session, "POST", "/api/v1/stacks/"+stack+"/versions", map[string]any{"config": serviceConfig()}, newServiceID(t), initial.ETag)
	newVersion := replyMap(t, saved)["version_id"].(string)
	_, e := s.db.Exec(`UPDATE config_versions SET (package_cipher,wrapped_dek,key_id,fingerprint)=(SELECT package_cipher,wrapped_dek,key_id,fingerprint FROM config_versions WHERE id=?) WHERE id=?`, newVersion, old)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DoService(context.Background(), l.Session, ServiceRequest{Method: "GET", Path: "/api/v1/stacks/" + stack + "/versions/" + old})
	if !errors.Is(e, ErrEvidence) {
		t.Fatalf("swapped ciphertext: %v", e)
	}
}

func TestReviewM1BusyFailureClassification(t *testing.T) {
	s, l, _, path := serviceFixture(t)
	ctx := context.Background()
	other, e := openDB(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	blocker, e := other.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	_, e = s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/nodes", Body: json.RawMessage(`{"display_name":"busy review"}`), IdempotencyKey: newServiceID(t)})
	var se *ServiceError
	if !errors.As(e, &se) || se.Status != 503 || se.Code != "storage_busy" {
		t.Fatalf("expected 503 storage_busy for locked SQLite, got %T %v", e, e)
	}
}

func TestReviewM1FullSaveIsClassifiedAndAtomic(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	node := serviceNodeWithGrants(t, s, l)
	ctx := context.Background()
	var pages int
	if e := s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); e != nil {
		t.Fatal(e)
	}
	config := serviceConfig()
	config["files"] = []any{map[string]any{"path": "compose.yaml", "content_b64": "c2VydmljZXM6IHt9Cg=="}}
	// Many uncompressed data bytes force SQLite to allocate beyond the imposed budget.
	content := bytes.Repeat([]byte("x"), 100000)
	config["interpolation_env"] = map[string]string{"LONG_VALUE": string(content)}
	raw, _ := json.Marshal(map[string]any{"project_name": "full_review", "config": config})
	var receiptsBefore, auditsBefore int
	s.db.QueryRow(`SELECT count(*) FROM idempotency_records`).Scan(&receiptsBefore)
	s.db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&auditsBefore)
	_, e := s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/nodes/" + node + "/stacks", Body: raw, IdempotencyKey: newServiceID(t)})
	var se *ServiceError
	if !errors.As(e, &se) || se.Status != 503 || se.Code != "storage_full" {
		t.Fatalf("expected storage_full; got %T %v", e, e)
	}
	var stacks, versions, receipts, audits int
	s.db.QueryRow(`SELECT count(*) FROM stacks`).Scan(&stacks)
	s.db.QueryRow(`SELECT count(*) FROM config_versions`).Scan(&versions)
	s.db.QueryRow(`SELECT count(*) FROM idempotency_records`).Scan(&receipts)
	s.db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&audits)
	if stacks != 0 || versions != 0 || receipts != receiptsBefore || audits != auditsBefore {
		t.Fatal("partial full save", stacks, versions, receipts, audits)
	}
}
func TestReviewM1MissingCancelIsNotFound(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	_, e := s.DoService(context.Background(), l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/operations/" + newServiceID(t) + "/cancel"})
	var se *ServiceError
	if !errors.As(e, &se) || se.Status != 404 {
		t.Fatalf("cancel missing operation should be404, got %T %v", e, e)
	}
}

func TestReviewM1FullApplyClassificationAndAtomicity(t *testing.T) {
	s, l, _, _ := serviceFixture(t)
	node := serviceNodeWithGrants(t, s, l)
	ctx := context.Background()
	enrollment := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/nodes/"+node+"/enrollment-tokens", map[string]any{}, newServiceID(t), ""))
	ap := filepath.Join(t.TempDir(), "agent.db")
	agent, e := ledger.CreateRelease(ctx, ap, node, "review-agent", riAgentKey(), riJournalKey(), ledger.RecoveryOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer agent.Close()
	secret, _ := randomSecret()
	if _, e = s.EnrollNode(ctx, enrollment["token"].(string), EnrollmentRequest{node, agent.Identity().LedgerID, secret}); e != nil {
		t.Fatal(e)
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
	if e = agent.BindRecoverySessionFixture(ctx, session); e != nil {
		t.Fatal(e)
	}
	pair := &recoveryPair{m: s, a: agent, b: Binding{node, agent.Identity().LedgerID, session}, agentPath: ap, agentBoot: riReadAgentBoot(t, ap)}
	riScan(t, pair, "all", 10)
	riComplete(t, pair)
	config := serviceConfig()
	config["interpolation_env"] = map[string]string{"LONG_VALUE": string(bytes.Repeat([]byte("x"), 100000))}
	created := replyMap(t, serviceCall(t, s, l.Session, "POST", "/api/v1/nodes/"+node+"/stacks", map[string]any{"project_name": "full_apply_review", "config": config}, newServiceID(t), ""))
	var pages int
	s.db.QueryRow(`PRAGMA page_count`).Scan(&pages)
	if _, e = s.db.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]any{"version_id": created["version_id"]})
	_, e = s.DoService(ctx, l.Session, ServiceRequest{Method: "POST", Path: "/api/v1/stacks/" + created["stack_id"].(string) + "/apply", Body: raw, IdempotencyKey: newServiceID(t)})
	var ops, claims int
	s.db.QueryRow(`SELECT count(*) FROM operations`).Scan(&ops)
	s.db.QueryRow(`SELECT count(*) FROM resource_claims`).Scan(&claims)
	if ops != 0 || claims != 0 {
		t.Fatal("FULL apply partial write", ops, claims)
	}
	var se *ServiceError
	if !errors.As(e, &se) || se.Status != 503 || se.Code != "storage_full" {
		t.Fatalf("Apply FULL expected503 storage_full, got %T %v", e, e)
	}
}
