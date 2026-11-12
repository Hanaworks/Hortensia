package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/protocol"
)

// Only Agent-side binding/ready facts are fixtures. Manager enrollment,
// authentication, binding, recovery and Apply use the service entry points.
func serviceRecoveryPair(t *testing.T, now func() time.Time) (*recoveryPair, BrowserLogin, *AuthenticatedNode, Keys) {
	t.Helper()
	ctx := context.Background()
	s, login, keys, path := serviceFixture(t)
	node := serviceNodeWithGrants(t, s, login)
	enrollment := replyMap(t, serviceCall(t, s, login.Session, "POST", "/api/v1/nodes/"+node+"/enrollment-tokens", map[string]any{}, newServiceID(t), ""))
	ap := filepath.Join(t.TempDir(), "agent.db")
	agent, err := ledger.CreateRelease(ctx, ap, node, "service-recovery-agent", riAgentKey(), riJournalKey(), ledger.RecoveryOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	secret, err := randomSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnrollNode(ctx, enrollment["token"].(string), EnrollmentRequest{node, agent.Identity().LedgerID, secret}); err != nil {
		t.Fatal(err)
	}
	auth, err := s.AuthenticateNodeCredential(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	pair := &recoveryPair{m: s, a: agent, b: Binding{node, agent.Identity().LedgerID, newServiceID(t)}, path: path, agentPath: ap, agentBoot: riReadAgentBoot(t, ap)}
	serviceRecoveryBind(t, pair, auth)
	riScan(t, pair, "all", 10)
	riComplete(t, pair)
	return pair, login, auth, keys
}

func serviceRecoveryBind(t *testing.T, p *recoveryPair, auth *AuthenticatedNode) {
	t.Helper()
	profile := json.RawMessage(fmt.Sprintf(`{"docker_endpoint_id":%q,"policy_revision":1}`, newServiceID(t)))
	if err := p.m.BindAuthenticatedNodeSession(context.Background(), auth, p.b.SessionID, p.agentBoot, profile, []string{"compose.apply.v1", "config.bundle.v1"}); err != nil {
		t.Fatal(err)
	}
	if err := p.a.BindRecoverySessionFixture(context.Background(), p.b.SessionID); err != nil {
		t.Fatal(err)
	}
}

func serviceRecoveryApply(t *testing.T, p *recoveryPair, login BrowserLogin, project string) Operation {
	t.Helper()
	created := replyMap(t, serviceCall(t, p.m, login.Session, "POST", "/api/v1/nodes/"+p.b.NodeID+"/stacks", map[string]any{"project_name": project, "config": serviceConfig()}, newServiceID(t), ""))
	accepted := replyMap(t, serviceCall(t, p.m, login.Session, "POST", "/api/v1/stacks/"+created["stack_id"].(string)+"/apply", map[string]any{"version_id": created["version_id"]}, newServiceID(t), ""))
	op, err := p.m.Get(context.Background(), accepted["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := p.m.ReadPayload(context.Background(), op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(payload)
	prepare, err := protocol.NewMessage("prepare", p.b.SessionID, "", prepareFields{Operation: op.OperationID, Digest: op.Digest, Node: op.NodeID, Stack: op.StackID, Version: op.VersionID, Payload: base64.StdEncoding.EncodeToString(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.a.RecordReadyFixture(context.Background(), prepare); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestServiceRecoveryReadyPersistsOnlyCancelIntent(t *testing.T) {
	p, login, _, _ := serviceRecoveryPair(t, nil)
	ctx := context.Background()
	op := serviceRecoveryApply(t, p, login, "recovery_cancel")
	scan := riScan(t, p, "all", 1)
	if scan.Seen != 1 {
		t.Fatalf("missing real Agent record: %+v", scan)
	}
	for range 2 {
		state, err := p.m.ProcessRecovery(ctx, p.b)
		if err != nil || state.Status != "waiting" {
			t.Fatalf("ready record should persist cancel and wait: %+v %v", state, err)
		}
	}
	got, err := p.m.Get(ctx, op.OperationID)
	if err != nil || !got.CancelRequested || got.State != "accepted" || got.ReleaseStatus != "held" || got.LatestResultSeq != 0 || got.MaxEventSeq != 0 || got.PermitID != "" || got.Revision != op.Revision+1 {
		t.Fatalf("cancellation invented outcome or was not idempotent: %+v %v", got, err)
	}
	claims, err := p.m.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("cancel released claims: %+v %v", claims, err)
	}
	intents, err := p.m.Intents(ctx, op.OperationID)
	if err != nil || len(intents) != 2 || intents[0].Kind != "cancel" || intents[0].State != "pending" || intents[1].Kind != "prepare" || intents[1].State != "suppressed" {
		t.Fatalf("cancel delivery intent: %+v %v", intents, err)
	}
	if mfCount(t, p.m, "operation_events") != 0 || mfCount(t, p.m, "absence_releases") != 0 {
		t.Fatal("cancel invented event or absence proof")
	}
	riFrozen(t, p.m, p.b.NodeID, true)
	if _, err = p.m.PrepareRecoveryComplete(ctx, p.b); !errors.Is(err, ErrRecoveryIncomplete) {
		t.Fatalf("cancel completed recovery without terminal receipt: %v", err)
	}
	if err = p.m.RequestCancelFixture(ctx, op.OperationID); !errors.Is(err, ErrSchema) {
		t.Fatalf("v4 Fixture was enabled: %v", err)
	}
}

// Agent Reconcile returns typed failures; the future transport maps them to
// error wires. This helper creates only that controlled protocol boundary.
func serviceRecoveryErrorWire(t *testing.T, p *recoveryPair, request []byte, scan any, code string) []byte {
	t.Helper()
	env := mfEnvelope(t, request)
	var req struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(env.Body, &req); err != nil {
		t.Fatal(err)
	}
	raw, err := protocol.NewMessage("reconcile_reply", p.b.SessionID, env.MessageID, map[string]any{"scope": req.Scope, "scan_id": scan, "ledger_id": p.b.LedgerID, "total": 0, "records": []any{}, "next_cursor": nil, "complete": false, "error": map[string]any{"code": code, "message": "controlled scan failure", "category": "unavailable", "retry_action": "same_request"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.m.validator.Validate(raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestServiceRecoveryErrorReplyKeepsCurrentRequestAndFreeze(t *testing.T) {
	p, _, _, _ := serviceRecoveryPair(t, nil)
	ctx := context.Background()
	for _, code := range []string{"busy", "scan_expired", "scan_changed", "cursor_invalid"} {
		t.Run(code, func(t *testing.T) {
			request, err := p.m.StartRecovery(ctx, p.b, "all", 1)
			if err != nil {
				t.Fatal(err)
			}
			before, err := p.m.ReadRecoveryState(ctx, p.b.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			wrong := serviceRecoveryErrorWire(t, p, request, newServiceID(t), code)
			if _, err = p.m.ReceiveRecoveryPage(ctx, p.b, wrong); !errors.Is(err, ErrRecovery) {
				t.Fatalf("mismatched scan changed state: %v", err)
			}
			state, err := p.m.ReceiveRecoveryPage(ctx, p.b, serviceRecoveryErrorWire(t, p, request, nil, code))
			want := "expired"
			if code == "busy" {
				want = "collecting"
			}
			if err != nil || state.Status != want || state.RoundID != before.RoundID {
				t.Fatalf("%s reply: %+v %v", code, state, err)
			}
			riFrozen(t, p.m, p.b.NodeID, true)
			if code == "busy" {
				retry, err := p.m.NextRecoveryRequest(ctx, p.b)
				if err != nil || !bytes.Equal(request, retry) {
					t.Fatalf("busy lost original request: %v", err)
				}
			} else {
				fresh := riScan(t, p, "all", 1)
				if fresh.RoundID == before.RoundID {
					t.Fatal("expired round reused")
				}
				if _, err = p.m.ReceiveRecoveryPage(ctx, p.b, serviceRecoveryErrorWire(t, p, request, nil, code)); !errors.Is(err, ErrRecovery) {
					t.Fatalf("old error changed new round: %v", err)
				}
				riComplete(t, p)
			}
			if _, err = p.m.RecordRecoveryFailureFixture(ctx, p.b, mfEnvelope(t, request).MessageID, code); !errors.Is(err, ErrSchema) {
				t.Fatalf("v4 failure Fixture enabled: %v", err)
			}
		})
	}
}

func TestServiceRecoveryRealAgentExpiredPageCanRestart(t *testing.T) {
	now := time.Now()
	p, login, _, _ := serviceRecoveryPair(t, func() time.Time { return now })
	serviceRecoveryApply(t, p, login, "expired_one")
	serviceRecoveryApply(t, p, login, "expired_two")
	ctx := context.Background()
	request, err := p.m.StartRecovery(ctx, p.b, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := riExchangePage(t, p, request)
	request, err = p.m.NextRecoveryRequest(ctx, p.b)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Minute)
	if raw, err := p.a.Reconcile(ctx, request); !errors.Is(err, ledger.ErrScanExpired) || len(raw) != 0 {
		t.Fatalf("Agent did not expire: %v", err)
	}
	state, err := p.m.ReceiveRecoveryPage(ctx, p.b, serviceRecoveryErrorWire(t, p, request, first.ScanID, "scan_expired"))
	if err != nil || state.Status != "expired" || state.Pages != 1 || state.Seen != 1 {
		t.Fatalf("expired page lost prior progress: %+v %v", state, err)
	}
	fresh := riScan(t, p, "all", 1)
	if fresh.RoundID == first.RoundID || fresh.ScanID == first.ScanID || fresh.Seen != 2 {
		t.Fatalf("fresh Agent scan was not accepted: %+v", fresh)
	}
	riFrozen(t, p.m, p.b.NodeID, true)
}

func TestServiceRecoveryCancelRechecksCurrentTransaction(t *testing.T) {
	for _, change := range []string{"round", "session", "manager_boot", "permit", "result", "collecting", "completed"} {
		t.Run(change, func(t *testing.T) {
			p, login, auth, keys := serviceRecoveryPair(t, nil)
			ctx := context.Background()
			op := serviceRecoveryApply(t, p, login, "recheck_"+change)
			if change == "permit" {
				ready := serviceRecoveryReadyWire(t, p, op)
				if err := p.m.ReceiveReady(ctx, auth, p.b.SessionID, ready); err != nil {
					t.Fatal(err)
				}
				start, err := protocol.NewMessage("request_start", p.b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": newServiceID(t)})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = p.m.AuthorizeStart(ctx, p.b, start); err != nil {
					t.Fatal(err)
				}
			}
			if change == "result" {
				if _, err := p.m.ReceiveEvent(ctx, p.b, eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "preflight_failed")); err != nil {
					t.Fatal(err)
				}
			}
			var scan RecoveryState
			var err error
			if change == "completed" {
				scan, err = p.m.ReadRecoveryState(ctx, p.b.NodeID)
			} else if change == "collecting" {
				_, err = p.m.StartRecovery(ctx, p.b, "all", 1)
				if err == nil {
					scan, err = p.m.ReadRecoveryState(ctx, p.b.NodeID)
				}
			} else {
				scope := "all"
				if change == "round" {
					scope = "pending"
				}
				scan = riScan(t, p, scope, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "round":
				fresh := riScan(t, p, "all", 1)
				if fresh.RoundID == scan.RoundID {
					t.Fatal("round not replaced")
				}
			case "session":
				replacement := *p
				replacement.b.SessionID = newServiceID(t)
				serviceRecoveryBind(t, &replacement, auth)
			case "manager_boot":
				current, err := OpenService(ctx, p.path, p.m.Identity(), keys)
				if err != nil {
					t.Fatal(err)
				}
				defer current.Close()
			}
			before, err := p.m.Get(ctx, op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.m.recoveryCancel(ctx, p.b, scan.RoundID, op.OperationID); err == nil {
				t.Fatalf("%s accepted obsolete recovery cancellation", change)
			}
			after, err := p.m.Get(ctx, op.OperationID)
			if err != nil || after != before {
				t.Fatalf("%s changed operation: before=%+v after=%+v %v", change, before, after, err)
			}
			intents, err := p.m.Intents(ctx, op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			for _, intent := range intents {
				if intent.Kind == "cancel" {
					t.Fatal("obsolete cancellation published intent")
				}
			}
			claims, err := p.m.Claims(ctx, op.OperationID)
			if err != nil || len(claims) != 2 {
				t.Fatalf("rejected cancellation released claims: %v", err)
			}
		})
	}
}

func TestServiceRecoveryCancelFailureRollsBackIntentAndFlag(t *testing.T) {
	p, login, _, _ := serviceRecoveryPair(t, nil)
	ctx := context.Background()
	op := serviceRecoveryApply(t, p, login, "cancel_rollback")
	riScan(t, p, "all", 1)
	if _, err := p.m.db.Exec(`CREATE TRIGGER fail_recovery_cancel BEFORE INSERT ON outbox WHEN NEW.kind='cancel' BEGIN SELECT RAISE(ABORT,'controlled cancel failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.m.ProcessRecovery(ctx, p.b); !errors.Is(err, ErrStorage) {
		t.Fatalf("cancel insert failure: %v", err)
	}
	got, err := p.m.Get(ctx, op.OperationID)
	if err != nil || got.CancelRequested || got.Revision != op.Revision {
		t.Fatalf("partial cancel: %+v %v", got, err)
	}
	intents, err := p.m.Intents(ctx, op.OperationID)
	if err != nil || len(intents) != 1 || intents[0].Kind != "prepare" || intents[0].State != "pending" {
		t.Fatalf("partial intent: %+v %v", intents, err)
	}
	if _, err = p.m.db.Exec(`DROP TRIGGER fail_recovery_cancel`); err != nil {
		t.Fatal(err)
	}
	state, err := p.m.ProcessRecovery(ctx, p.b)
	if err != nil || state.Status != "waiting" {
		t.Fatalf("retry after cancel failure: %+v %v", state, err)
	}
}

func serviceRecoveryReadyWire(t *testing.T, p *recoveryPair, op Operation) []byte {
	t.Helper()
	ready := ctlFixture(t, "ready")
	ready["session_id"] = p.b.SessionID
	body := ready["body"].(map[string]any)
	body["operation_id"], body["input_digest"] = op.OperationID, op.Digest
	raw, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestServiceRecoveryPreparingAndReadyCancelWithExhaustedKeyBudget(t *testing.T) {
	for _, phase := range []string{"preparing", "ready"} {
		t.Run(phase, func(t *testing.T) {
			p, login, auth, _ := serviceRecoveryPair(t, nil)
			ctx := context.Background()
			op := serviceRecoveryApply(t, p, login, "phase_"+phase)
			if err := p.m.ReceiveReady(ctx, auth, p.b.SessionID, serviceRecoveryReadyWire(t, p, op)); err != nil {
				t.Fatal(err)
			}
			r := rootRecordFor(op)
			r["phase"] = phase
			rootReceiveRecord(t, p, r)
			if _, err := p.m.db.Exec(`UPDATE keys SET uses=?`, maxKeyUses); err != nil {
				t.Fatal(err)
			}
			state, err := p.m.ProcessRecovery(ctx, p.b)
			if err != nil || state.Status != "waiting" {
				t.Fatalf("phase %s cancel: %+v %v", phase, state, err)
			}
			got, err := p.m.Get(ctx, op.OperationID)
			if err != nil || !got.CancelRequested || got.State != "ready" || got.ReleaseStatus != "held" || got.MaxEventSeq != 0 {
				t.Fatalf("phase %s invented outcome: %+v %v", phase, got, err)
			}
			var uses int64
			if err = p.m.db.QueryRow(`SELECT uses FROM keys`).Scan(&uses); err != nil || uses != maxKeyUses {
				t.Fatalf("cancel consumed key budget: %d %v", uses, err)
			}
			intents, err := p.m.Intents(ctx, op.OperationID)
			if err != nil || len(intents) != 2 || intents[0].Kind != "cancel" || intents[0].State != "pending" || intents[1].Kind != "prepare" || intents[1].State != "confirmed" {
				t.Fatalf("ready cancel intents: %+v %v", intents, err)
			}
		})
	}
}
