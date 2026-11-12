package operations

import (
	"bytes"
	"context"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/protocol"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRuntimeMV01ExplicitFormatAndFixtureBoundary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.db")
	s, e := CreateRuntimeService(ctx, path, storageManagerID, storageKeys(), Bootstrap{"administrator", "runtime-test-password"})
	if e != nil {
		t.Fatal(e)
	}
	identity := s.Identity()
	if e = s.CreateUserFixture(ctx, storageManagerID, true); !errors.Is(e, ErrSchema) {
		t.Fatal("fixture admitted", e)
	}
	if _, e = s.Login(ctx, "administrator", "runtime-test-password"); e != nil {
		t.Fatal("formal login", e)
	}
	s.Close()
	if _, e = OpenService(ctx, path, identity, storageKeys()); !errors.Is(e, ErrSchema) {
		t.Fatal("v4 opened v5", e)
	}
	reopened, e := OpenRuntimeService(ctx, path, identity, storageKeys())
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
}

func runtimePair(t *testing.T) (*recoveryPair, BrowserLogin, *AuthenticatedNode, Keys) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.db")
	keys := storageKeys()
	s, e := CreateRuntimeService(ctx, path, storageManagerID, keys, Bootstrap{"admin", "correct horse battery"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	login, e := s.Login(ctx, "admin", "correct horse battery")
	if e != nil {
		t.Fatal(e)
	}
	node := serviceNodeWithGrants(t, s, login)
	enrollment := replyMap(t, serviceCall(t, s, login.Session, "POST", "/api/v1/nodes/"+node+"/enrollment-tokens", map[string]any{}, newServiceID(t), ""))
	agentPath := filepath.Join(t.TempDir(), "agent.db")
	agent, e := ledger.CreateRelease(ctx, agentPath, node, "runtime-test-agent", riAgentKey(), riJournalKey(), ledger.RecoveryOptions{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { agent.Close() })
	secret, _ := randomSecret()
	if _, e = s.EnrollNode(ctx, enrollment["token"].(string), EnrollmentRequest{node, agent.Identity().LedgerID, secret}); e != nil {
		t.Fatal(e)
	}
	auth, e := s.AuthenticateNodeCredential(ctx, secret)
	if e != nil {
		t.Fatal(e)
	}
	boot := riReadAgentBoot(t, agentPath)
	hello, e := protocol.NewMessage("hello", "", "", map[string]any{"node_id": node, "ledger_id": agent.Identity().LedgerID, "boot_id": boot, "agent_version": "runtime-test", "compose_version": "runtime-test", "capabilities": []string{"compose.apply.v1", "config.bundle.v1"}, "execution_profile": map[string]any{"docker_endpoint_id": newServiceID(t), "policy_revision": 1}})
	if e != nil {
		t.Fatal(e)
	}
	b, welcome, e := s.BindAgentHello(ctx, auth, hello)
	if e != nil {
		t.Fatal(e)
	}
	env, e := s.validator.Validate(welcome)
	if e != nil || env.ReplyTo == nil {
		t.Fatal("welcome", e)
	}
	if e = agent.BindRecoverySessionFixture(ctx, b.SessionID); e != nil {
		t.Fatal(e)
	}
	p := &recoveryPair{m: s, a: agent, b: b, path: path, agentPath: agentPath, agentBoot: boot}
	riScan(t, p, "all", 10)
	riComplete(t, p)
	return p, login, auth, keys
}
func runtimeApply(t *testing.T, p *recoveryPair, login BrowserLogin) Operation {
	t.Helper()
	created := replyMap(t, serviceCall(t, p.m, login.Session, "POST", "/api/v1/nodes/"+p.b.NodeID+"/stacks", map[string]any{"project_name": "runtime_project", "config": serviceConfig()}, newServiceID(t), ""))
	accepted := replyMap(t, serviceCall(t, p.m, login.Session, "POST", "/api/v1/stacks/"+created["stack_id"].(string)+"/apply", map[string]any{"version_id": created["version_id"]}, newServiceID(t), ""))
	op, e := p.m.Get(context.Background(), accepted["operation_id"].(string))
	if e != nil {
		t.Fatal(e)
	}
	return op
}
func runtimeAuthorize(t *testing.T, p *recoveryPair, auth *AuthenticatedNode, op Operation) Operation {
	t.Helper()
	ctx := context.Background()
	fixture := ctlFixture(t, "ready")
	fixture["session_id"] = p.b.SessionID
	fixture["body"].(map[string]any)["operation_id"] = op.OperationID
	fixture["body"].(map[string]any)["input_digest"] = op.Digest
	ready, _ := json.Marshal(fixture)
	if e := p.m.ReceiveReady(ctx, auth, p.b.SessionID, ready); e != nil {
		t.Fatal(e)
	}
	start, _ := protocol.NewMessage("request_start", p.b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": newServiceID(t)})
	if _, e := p.m.AuthorizeStart(ctx, p.b, start); e != nil {
		t.Fatal(e)
	}
	op, e := p.m.Get(ctx, op.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	return op
}
func runtimeSeq(t *testing.T, raw []byte, seq uint64) []byte {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	m["body"].(map[string]any)["event_seq"] = seq
	m["message_id"] = newServiceID(t)
	out, _ := json.Marshal(m)
	return out
}

func TestRuntimeMV03DeliveryWaitIsNotBusinessConfirmation(t *testing.T) {
	p, login, auth, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeApply(t, p, login)
	now := time.Now()
	first, e := p.m.NextNodeDelivery(ctx, p.b, now)
	if e != nil || first == nil || first.Kind != "prepare" {
		t.Fatal("first delivery", first, e)
	}
	second, e := p.m.NextNodeDelivery(ctx, p.b, now.Add(999*time.Millisecond))
	if e != nil || second != nil {
		t.Fatal("ignored wait", second, e)
	}
	second, e = p.m.NextNodeDelivery(ctx, p.b, now.Add(time.Second))
	if e != nil || second == nil || !bytes.Equal(first.Raw, second.Raw) {
		t.Fatal("unstable retry", second, e)
	}
	intents, e := p.m.Intents(ctx, op.OperationID)
	if e != nil || intents[0].State != "pending" {
		t.Fatal("write confirmed business", intents, e)
	}
	if e = p.m.RecordDeliveryFailure(ctx, p.b, first.MessageID, "write_failed", now); e != nil {
		t.Fatal(e)
	}
	runtimeAuthorize(t, p, auth, op)
	third, e := p.m.NextNodeDelivery(ctx, p.b, now.Add(time.Minute))
	if e != nil || third != nil {
		t.Fatal("ready did not confirm prepare", third, e)
	}
	old := p.b
	p.b.SessionID = newServiceID(t)
	if e = p.m.BindAuthenticatedNodeSession(ctx, auth, p.b.SessionID, p.agentBoot, json.RawMessage(fmt.Sprintf(`{"docker_endpoint_id":%q,"policy_revision":1}`, op.EndpointID)), []string{"compose.apply.v1", "config.bundle.v1"}); e != nil {
		t.Fatal(e)
	}
	if _, e = p.m.NextNodeDelivery(ctx, old, now.Add(time.Minute)); !errors.Is(e, ErrFrozen) {
		t.Fatal("old session delivery", e)
	}
	if e = p.m.RecordDeliveryFailure(ctx, old, first.MessageID, "write_failed", now); !errors.Is(e, ErrFrozen) {
		t.Fatal("old failure", e)
	}
}
func TestRuntimeMV04StartedThenNotSpawnedWaitsForFinalized(t *testing.T) {
	p, login, auth, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	eventReceive(t, p.m, p.b, eventStarted(t, p.m, p.b, op))
	eventReceive(t, p.m, p.b, runtimeSeq(t, eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "canceled_before_start"), 2))
	got, e := p.m.Get(ctx, op.OperationID)
	if e != nil || got.State != "canceled" || got.LatestResultSeq != 2 || got.ReleaseStatus != "pending" {
		t.Fatal(got, e)
	}
	claims, e := p.m.Claims(ctx, op.OperationID)
	if e != nil || len(claims) != 2 {
		t.Fatal(claims, e)
	}
	raw, e := p.m.PrepareFinalize(ctx, p.b, op.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	env, _ := p.m.validator.Validate(raw)
	response, _ := protocol.NewMessage("finalized", p.b.SessionID, env.MessageID, json.RawMessage(env.Body))
	if e = p.m.ReceiveFinalized(ctx, p.b, response); e != nil {
		t.Fatal(e)
	}
	claims, e = p.m.Claims(ctx, op.OperationID)
	if e != nil || len(claims) != 0 {
		t.Fatal("not released", claims, e)
	}
}
func TestRuntimeMV05OutOfOrderObservationChainAndImmutableProcess(t *testing.T) {
	p, login, auth, keys := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	eventReceive(t, p.m, p.b, eventStarted(t, p.m, p.b, op))
	old := eventResult(t, p.m, p.b, op, "normal_exit", 0, "error", "unknown", "")
	newer := runtimeSeq(t, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""), 3)
	eventReceive(t, p.m, p.b, newer)
	got, e := p.m.Get(ctx, op.OperationID)
	if e != nil || got.LatestResultSeq != 0 {
		t.Fatal("unproven chain became latest", got, e)
	}
	if _, e = p.m.PrepareFinalize(ctx, p.b, op.OperationID); e == nil {
		t.Fatal("pending chain finalized")
	}
	eventReceive(t, p.m, p.b, old)
	got, e = p.m.Get(ctx, op.OperationID)
	if e != nil || got.LatestResultSeq != 3 || got.State != "succeeded" {
		t.Fatal("chain not promoted", got, e)
	}
	eventReceive(t, p.m, p.b, old)
	reopened, e := OpenRuntimeService(ctx, p.path, p.m.Identity(), keys)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	got, e = reopened.Get(ctx, op.OperationID)
	if e != nil || got.LatestResultSeq != 3 {
		t.Fatal("reopen lost chain", got, e)
	}
}
func TestRuntimeMV05ProcessRewriteAndForbiddenRepairAreIsolated(t *testing.T) {
	for _, mode := range []string{"output", "exit_code", "finished_at", "interrupted", "drifted"} {
		t.Run(mode, func(t *testing.T) {
			p, login, auth, _ := runtimePair(t)
			ctx := context.Background()
			op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
			eventReceive(t, p.m, p.b, eventStarted(t, p.m, p.b, op))
			outcome, ownership := "normal_exit", "unknown"
			if mode == "interrupted" {
				outcome = "interrupted"
			}
			if mode == "drifted" {
				ownership = "drifted"
			}
			old := eventResult(t, p.m, p.b, op, outcome, 0, "error", ownership, "")
			eventReceive(t, p.m, p.b, old)
			newer := runtimeSeq(t, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""), 3)
			var m map[string]any
			json.Unmarshal(newer, &m)
			body := m["body"].(map[string]any)
			switch mode {
			case "output":
				body["output_b64"] = "YWx0ZXJlZA=="
			case "finished_at":
				body["finished_at"] = "2026-11-07T04:02:00.000Z"
			case "exit_code":
				body["exit_code"] = 8
				body["error"] = map[string]any{"category": "execution_failed", "code": "compose_exit_nonzero", "message": "controlled fixture", "retry_action": "query_operation"}
			}
			newer, _ = json.Marshal(m)
			if _, e := p.m.ReceiveEvent(ctx, p.b, newer); !errors.Is(e, ErrConflict) {
				t.Fatal("rewritten process accepted", e)
			}
			got, e := p.m.Get(ctx, op.OperationID)
			if e != nil || !got.Blocked || got.LatestResultSeq != 2 || got.ReleaseStatus != "held" {
				t.Fatal("not isolated", got, e)
			}
		})
	}
}
func TestRuntimeMV06FinalizationAuthenticatesLatestAndLateHistory(t *testing.T) {
	p, login, auth, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	// Result may arrive before the compatible started event.
	old := eventResult(t, p.m, p.b, op, "normal_exit", 0, "error", "unknown", "")
	eventReceive(t, p.m, p.b, old)
	observe, e := p.m.NextNodeDelivery(ctx, p.b, time.Now())
	if e != nil || observe == nil || observe.Kind != "observe" {
		t.Fatal("missing repair", observe, e)
	}
	eventReceive(t, p.m, p.b, runtimeSeq(t, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""), 3))
	finalize, e := p.m.NextNodeDelivery(ctx, p.b, time.Now())
	if e != nil || finalize == nil || finalize.Kind != "finalize" {
		t.Fatal("missing finalize", finalize, e)
	}
	env, _ := p.m.validator.Validate(finalize.Raw)
	var body map[string]any
	json.Unmarshal(env.Body, &body)
	if body["result_event_seq"] != float64(3) {
		t.Fatal("wrong final seq", body)
	}
	response, _ := protocol.NewMessage("finalized", p.b.SessionID, env.MessageID, json.RawMessage(env.Body))
	if e = p.m.ReceiveFinalized(ctx, p.b, response); e != nil {
		t.Fatal(e)
	}
	eventReceive(t, p.m, p.b, old)
	eventReceive(t, p.m, p.b, eventStarted(t, p.m, p.b, op))
	claims, e := p.m.Claims(ctx, op.OperationID)
	if e != nil || len(claims) != 0 {
		t.Fatal("late event recreated claims", claims, e)
	}
	if _, e = p.m.db.Exec(`UPDATE finalization SET result_event_seq=2 WHERE operation_id=?`, op.OperationID); e != nil {
		t.Fatal(e)
	}
	if _, e = p.m.Get(ctx, op.OperationID); !errors.Is(e, ErrEvidence) {
		t.Fatal("corrupt final seq trusted", e)
	}
}
func TestRuntimeMV07RecoveryCancellationAndDeliveryFreeze(t *testing.T) {
	p, login, _, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeApply(t, p, login)
	payload, e := p.m.ReadPayload(ctx, op.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	prepare, _ := protocol.NewMessage("prepare", p.b.SessionID, "", prepareFields{op.OperationID, op.Digest, op.NodeID, op.StackID, op.VersionID, base64.StdEncoding.EncodeToString(payload)})
	clear(payload)
	if _, e = p.a.RecordReadyFixture(ctx, prepare); e != nil {
		t.Fatal(e)
	}
	riScan(t, p, "all", 10)
	d, e := p.m.NextNodeDelivery(ctx, p.b, time.Now())
	if e != nil || d != nil {
		t.Fatal("prepare during recovery", d, e)
	}
	state, e := p.m.ProcessRecovery(ctx, p.b)
	if e != nil || state.Status != "waiting" {
		t.Fatal(state, e)
	}
	d, e = p.m.NextNodeDelivery(ctx, p.b, time.Now())
	if e != nil || d == nil || d.Kind != "cancel" {
		t.Fatal("cancel unavailable in recovery", d, e)
	}
	got, e := p.m.Get(ctx, op.OperationID)
	if e != nil || !got.CancelRequested || got.LatestResultSeq != 0 || got.ReleaseStatus != "held" {
		t.Fatal("cancel invented result", got, e)
	}
	if _, e = p.m.PrepareRecoveryComplete(ctx, p.b); !errors.Is(e, ErrRecoveryIncomplete) {
		t.Fatal("completed without finalized", e)
	}
}
func TestRuntimeMV03EncryptedDeliveryAndTamperRefusal(t *testing.T) {
	p, login, _, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeApply(t, p, login)
	now := time.Now()
	d, e := p.m.NextNodeDelivery(ctx, p.b, now)
	if e != nil || d == nil {
		t.Fatal(d, e)
	}
	env, _ := p.m.validator.Validate(d.Raw)
	var body prepareFields
	json.Unmarshal(env.Body, &body)
	for _, path := range []string{p.path, p.path + "-wal"} {
		raw, e := os.ReadFile(path)
		if e != nil && !os.IsNotExist(e) {
			t.Fatal(e)
		}
		if bytes.Contains(raw, []byte(body.Payload)) || bytes.Contains(raw, d.Raw) {
			t.Fatal("prepare payload persisted plaintext")
		}
	}
	var cipher []byte
	if e = p.m.db.QueryRow(`SELECT body_cipher FROM runtime_deliveries WHERE operation_id=?`, op.OperationID).Scan(&cipher); e != nil {
		t.Fatal(e)
	}
	cipher[len(cipher)-1] ^= 1
	if _, e = p.m.db.Exec(`UPDATE runtime_deliveries SET body_cipher=? WHERE operation_id=?`, cipher, op.OperationID); e != nil {
		t.Fatal(e)
	}
	if _, e = p.m.NextNodeDelivery(ctx, p.b, now.Add(time.Minute)); !errors.Is(e, ErrEvidence) {
		t.Fatal("tampered delivery sent", e)
	}
}
func TestRuntimeMV05PendingPromotionPreservesExactBody(t *testing.T) {
	p, login, auth, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	old := eventResult(t, p.m, p.b, op, "normal_exit", 0, "error", "unknown", "")
	newer := runtimeSeq(t, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""), 3)
	// Whitespace is authenticated evidence too; promotion must not re-encode it.
	newer = bytes.Replace(newer, []byte(`"body":{`), []byte("\"body\":{\n  "), 1)
	env, e := p.m.validator.Validate(newer)
	if e != nil {
		t.Fatal(e)
	}
	exact := append([]byte(nil), env.Body...)
	eventReceive(t, p.m, p.b, newer)
	eventReceive(t, p.m, p.b, old)
	stored, e := p.m.ReadEvent(ctx, op.OperationID, 3)
	if e != nil || !bytes.Equal(exact, stored.Body) {
		t.Fatal("promotion changed original body", e)
	}
}
func TestRuntimeMV06NewRoundReplacesDeliveryWithinSameSession(t *testing.T) {
	p, login, auth, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	eventReceive(t, p.m, p.b, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""))
	now := time.Now()
	first, e := p.m.NextNodeDelivery(ctx, p.b, now)
	if e != nil || first == nil || first.Kind != "finalize" {
		t.Fatal(first, e)
	}
	if _, e = p.m.StartRecovery(ctx, p.b, "all", 10); e != nil {
		t.Fatal(e)
	}
	second, e := p.m.NextNodeDelivery(ctx, p.b, now)
	if e != nil || second == nil || second.Kind != "finalize" || second.MessageID == first.MessageID {
		t.Fatal("old round request reused", second, e)
	}
	if e = p.m.RecordDeliveryFailure(ctx, p.b, first.MessageID, "write_failed", now); !errors.Is(e, ErrNotFound) {
		t.Fatal("old round failure accepted", e)
	}
}
func TestRuntimeMV02HelloIdentityAndAllFixtureRejection(t *testing.T) {
	p, _, auth, _ := runtimePair(t)
	ctx := context.Background()
	var profileText string
	if e := p.m.db.QueryRow(`SELECT execution_profile_json FROM nodes WHERE id=?`, p.b.NodeID).Scan(&profileText); e != nil {
		t.Fatal(e)
	}
	hello, _ := protocol.NewMessage("hello", "", "", map[string]any{"node_id": newServiceID(t), "ledger_id": p.b.LedgerID, "boot_id": p.agentBoot, "agent_version": "test", "compose_version": "test", "capabilities": []string{}, "execution_profile": json.RawMessage(profileText)})
	if _, _, e := p.m.BindAgentHello(ctx, auth, hello); !errors.Is(e, ErrIdentity) {
		t.Fatal("foreign hello accepted", e)
	}
	if e := p.m.requireBinding(ctx, p.m.db, p.b, false); e != nil {
		t.Fatal("failed hello replaced session", e)
	}
	for _, call := range []func() error{
		func() error { return p.m.CreateUserFixture(ctx, newServiceID(t), true) }, func() error { return p.m.SetUserEnabledFixture(ctx, newServiceID(t), true) }, func() error { return p.m.SetGrantFixture(ctx, newServiceID(t), p.b.NodeID, "node.read", true) }, func() error { return p.m.BindSessionFixture(ctx, p.b) }, func() error { return p.m.BindRecoverySessionFixture(ctx, p.b, p.agentBoot) }, func() error { return p.m.RecordReadyFixture(ctx, p.b, newServiceID(t), "") }, func() error { return p.m.RequestCancelFixture(ctx, newServiceID(t)) }, func() error { _, e := p.m.RecordRecoveryFailureFixture(ctx, p.b, newServiceID(t), "busy"); return e }, func() error { _, e := p.m.AcceptFixture(ctx, p.b, newServiceID(t), nil); return e },
	} {
		if e := call(); !errors.Is(e, ErrSchema) {
			t.Fatal("Fixture admitted v5", e)
		}
	}
}
func TestRuntimeMV03RetryBackoffAndConcurrentTake(t *testing.T) {
	p, login, _, _ := runtimePair(t)
	ctx := context.Background()
	runtimeApply(t, p, login)
	now := time.Now()
	var wg sync.WaitGroup
	results := make(chan *Delivery, 4)
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); d, e := p.m.NextNodeDelivery(ctx, p.b, now); results <- d; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	var first *Delivery
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for d := range results {
		if d != nil {
			if first != nil {
				t.Fatal("multiple simultaneous takes")
			}
			first = d
		}
	}
	if first == nil {
		t.Fatal("no take")
	}
	for _, seconds := range []int{1, 3, 7, 15, 31, 61, 91} {
		if d, e := p.m.NextNodeDelivery(ctx, p.b, now.Add(time.Duration(seconds)*time.Second-time.Millisecond)); e != nil || d != nil {
			t.Fatal("early retry", seconds, d, e)
		}
		d, e := p.m.NextNodeDelivery(ctx, p.b, now.Add(time.Duration(seconds)*time.Second))
		if e != nil || d == nil || !bytes.Equal(first.Raw, d.Raw) {
			t.Fatal("missing stable retry", seconds, d, e)
		}
	}
}

func TestRuntimeMV07ReleasedHistoryGetsCurrentRoundFinalize(t *testing.T) {
	p, login, _, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeApply(t, p, login)
	payload, e := p.m.ReadPayload(ctx, op.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	prepare, _ := protocol.NewMessage("prepare", p.b.SessionID, "", prepareFields{op.OperationID, op.Digest, op.NodeID, op.StackID, op.VersionID, base64.StdEncoding.EncodeToString(payload)})
	clear(payload)
	if _, e = p.a.RecordReadyFixture(ctx, prepare); e != nil {
		t.Fatal(e)
	}
	result, e := p.a.RecordResultFixture(ctx, eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "canceled_before_start"))
	if e != nil {
		t.Fatal(e)
	}
	riDeliverEvent(t, p, result)
	drFinalize(t, p, op.OperationID)
	drClaims(t, p, op.OperationID, 0, 0)
	riScan(t, p, "all", 10)
	state, e := p.m.ProcessRecovery(ctx, p.b)
	if e != nil || state.Status != "waiting" {
		t.Fatal(state, e)
	}
	d, e := p.m.NextNodeDelivery(ctx, p.b, time.Now())
	if e != nil || d == nil || d.Kind != "finalize" {
		t.Fatal("released history has no current round confirmation delivery", d, e)
	}
	response, e := p.a.Finalize(ctx, d.Raw)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.m.ReceiveFinalized(ctx, p.b, response); e != nil {
		t.Fatal(e)
	}
	state, e = p.m.ProcessRecovery(ctx, p.b)
	if e != nil || state.Status != "ready" {
		t.Fatal("current round did not converge", state, e)
	}
	riComplete(t, p)
}
func TestRuntimeMV03ExpiredObservationStopsOldQueryUntilNewContext(t *testing.T) {
	p, login, auth, _ := runtimePair(t)
	ctx := context.Background()
	op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
	eventReceive(t, p.m, p.b, eventResult(t, p.m, p.b, op, "normal_exit", 0, "error", "unknown", ""))
	now := time.Now()
	first, e := p.m.NextNodeDelivery(ctx, p.b, now)
	if e != nil || first == nil || first.Kind != "observe" {
		t.Fatal(first, e)
	}
	if e = p.m.RecordDeliveryFailure(ctx, p.b, first.MessageID, "cursor_expired", now); e != nil {
		t.Fatal(e)
	}
	if d, e := p.m.NextNodeDelivery(ctx, p.b, now.Add(time.Minute)); e != nil || d != nil {
		t.Fatal("expired query retried", d, e)
	}
	if _, e = p.m.StartRecovery(ctx, p.b, "all", 10); e != nil {
		t.Fatal(e)
	}
	second, e := p.m.NextNodeDelivery(ctx, p.b, now)
	if e != nil || second == nil || second.Kind != "observe" || bytes.Equal(first.Raw, second.Raw) {
		t.Fatal("new round did not replace query", second, e)
	}
}
