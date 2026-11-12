package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/protocol"
)

// The two real versioned stores exchange shared-schema messages. Only trusted
// identity, preparation and execution facts remain fixtures in this slice.
func drPair(t *testing.T) *recoveryPair {
	t.Helper()
	return drPairDir(t, t.TempDir())
}

func drPairDir(t *testing.T, dir string) *recoveryPair {
	t.Helper()
	ctx := context.Background()
	p := &recoveryPair{path: filepath.Join(dir, "manager.db"), agentPath: filepath.Join(dir, "agent.db")}
	var err error
	p.m, err = CreateRelease(ctx, p.path, mfManagerID, mfKeys())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.m.Close() })
	prepare := mfExample(t, "prepare")
	node := prepare["body"].(map[string]any)["node_id"].(string)
	p.a, err = ledger.CreateRelease(ctx, p.agentPath, node, "release-ledger", riAgentKey(), riJournalKey(), ledger.RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.a.Close() })
	p.b = Binding{NodeID: node, LedgerID: p.a.Identity().LedgerID, SessionID: prepare["session_id"].(string)}
	p.agentBoot = riReadAgentBoot(t, p.agentPath)
	riBind(t, p)
	if err = p.m.CreateUserFixture(ctx, mfUserID, true); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"node.read", "config.read", "stack.deploy"} {
		if err = p.m.SetGrantFixture(ctx, mfUserID, node, capability, true); err != nil {
			t.Fatal(err)
		}
	}
	riScan(t, p, "all", 10)
	riComplete(t, p)
	return p
}

func drClaims(t *testing.T, p *recoveryPair, id string, manager, agent int) {
	t.Helper()
	mc, me := p.m.Claims(context.Background(), id)
	ac, ae := p.a.Claims(context.Background(), id)
	if me != nil || ae != nil || len(mc) != manager || len(ac) != agent {
		t.Fatalf("claims manager=%d/%v agent=%d/%v want=%d/%d", len(mc), me, len(ac), ae, manager, agent)
	}
}

func drFinalize(t *testing.T, p *recoveryPair, id string) ([]byte, []byte) {
	t.Helper()
	ctx := context.Background()
	request, err := p.m.PrepareFinalize(ctx, p.b, id)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.m.PrepareFinalize(ctx, p.b, id)
	if err != nil || !bytes.Equal(request, again) {
		t.Fatalf("unstable finalize request %v", err)
	}
	response, err := p.a.Finalize(ctx, request)
	if err != nil {
		t.Fatalf("Agent finalize: %v", err)
	}
	q, r := mfEnvelope(t, request), mfEnvelope(t, response)
	var qb, rb map[string]any
	if json.Unmarshal(q.Body, &qb) != nil || json.Unmarshal(r.Body, &rb) != nil {
		t.Fatal("invalid finalize body")
	}
	if r.Type != "finalized" || r.ReplyTo == nil || *r.ReplyTo != q.MessageID || !reflect.DeepEqual(qb, rb) {
		t.Fatal("finalized lost fact/request correlation")
	}
	if err = p.m.ReceiveFinalized(ctx, p.b, response); err != nil {
		t.Fatalf("Manager finalized: %v", err)
	}
	return request, response
}

func TestDualReleaseOldSnapshotAndLostResponseConverge(t *testing.T) {
	for _, outcome := range []string{"not_started", "normal_exit"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			p := drPair(t)
			op, _ := riPrepare(t, p, 801, true)
			starts := 0
			if outcome == "normal_exit" {
				op = riAuthorize(t, p, op, &starts)
				started, err := p.a.ReadEvent(ctx, op.OperationID, 1)
				if err != nil {
					t.Fatal(err)
				}
				riDeliverEvent(t, p, started)
			}
			scan := riScan(t, p, "all", 1)
			page, err := p.m.ReadRecoveryPage(ctx, scan.RoundID, 0)
			if err != nil {
				t.Fatal(err)
			}
			observation, ownership, code := "", "not_checked", "canceled_before_start"
			if outcome == "normal_exit" {
				observation, ownership, code = "complete", "consistent", ""
			}
			result, err := p.a.RecordResultFixture(ctx, eventResult(t, p.m, p.b, op, outcome, 0, observation, ownership, code))
			if err != nil {
				t.Fatal(err)
			}
			riDeliverEvent(t, p, result)
			state, err := p.m.ProcessRecovery(ctx, p.b)
			if err != nil || state.Status != "waiting" {
				t.Fatalf("before release %+v %v", state, err)
			}
			request, err := p.m.PrepareFinalize(ctx, p.b, op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			response, err := p.a.Finalize(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			drClaims(t, p, op.OperationID, 2, 0)
			if _, err = p.m.PrepareRecoveryComplete(ctx, p.b); err == nil {
				t.Fatal("lost response opened recovery")
			}
			retry, err := p.a.Finalize(ctx, request)
			if err != nil || !bytes.Equal(mfEnvelope(t, response).Body, mfEnvelope(t, retry).Body) {
				t.Fatalf("lost reply replay %v", err)
			}
			if err = p.m.ReceiveFinalized(ctx, p.b, retry); err != nil {
				t.Fatal(err)
			}
			drClaims(t, p, op.OperationID, 0, 0)
			if err = p.m.ReceiveFinalized(ctx, p.b, response); err != nil {
				t.Fatalf("duplicate finalized %v", err)
			}
			// Original event replays remain acknowledgable after both sides release.
			riDeliverEvent(t, p, result)
			after, err := p.m.ReadRecoveryPage(ctx, scan.RoundID, 0)
			if err != nil || !bytes.Equal(page, after) {
				t.Fatalf("release rewrote snapshot %v", err)
			}
			state, err = p.m.ProcessRecovery(ctx, p.b)
			if err != nil || state.Status != "ready" {
				t.Fatalf("old page did not converge %+v %v", state, err)
			}
			riComplete(t, p)
			got, err := p.m.Get(ctx, op.OperationID)
			want := "canceled"
			if outcome == "normal_exit" {
				want = "succeeded"
			}
			if err != nil || got.State != want || got.ReleaseStatus != "released" {
				t.Fatalf("final state %+v %v", got, err)
			}
			if (outcome == "normal_exit" && starts != 1) || (outcome == "not_started" && starts != 0) {
				t.Fatalf("execution count=%d", starts)
			}
			a, ae := p.a.ReadFinalized(ctx, op.OperationID)
			m, me := p.m.ReadFinalized(ctx, op.OperationID)
			if ae != nil || me != nil || !bytes.Equal(a, m) {
				t.Fatalf("release evidence differs %v %v", ae, me)
			}
		})
	}
}

func TestDualReleaseReconnectRequiresCurrentRoundConfirmation(t *testing.T) {
	for _, change := range []string{"session", "manager", "agent"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			p := drPair(t)
			op, _ := riPrepare(t, p, 802, true)
			result, err := p.a.RecordResultFixture(ctx, eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "canceled_before_start"))
			if err != nil {
				t.Fatal(err)
			}
			riDeliverEvent(t, p, result)
			_, old := drFinalize(t, p, op.OperationID)
			switch change {
			case "session":
				p.b.SessionID = riID(18001)
			case "manager":
				id := p.m.Identity()
				if err = p.m.Close(); err != nil {
					t.Fatal(err)
				}
				p.m, err = OpenRelease(ctx, p.path, id, mfKeys())
				if err != nil {
					t.Fatal(err)
				}
			case "agent":
				id := p.a.Identity()
				if err = p.a.Close(); err != nil {
					t.Fatal(err)
				}
				p.a, err = ledger.OpenRelease(ctx, p.agentPath, id, riAgentKey(), riJournalKey(), ledger.RecoveryOptions{})
				if err != nil {
					t.Fatal(err)
				}
				p.agentBoot = riReadAgentBoot(t, p.agentPath)
			}
			riBind(t, p)
			riScan(t, p, "all", 10)
			if err = p.m.ReceiveFinalized(ctx, p.b, old); err == nil {
				t.Fatal("old release reply acquired new round qualification")
			}
			state, err := p.m.ProcessRecovery(ctx, p.b)
			if err != nil || state.Status != "waiting" {
				t.Fatalf("history bypassed current confirmation %+v %v", state, err)
			}
			drFinalize(t, p, op.OperationID)
			state, err = p.m.ProcessRecovery(ctx, p.b)
			if err != nil || state.Status != "ready" {
				t.Fatalf("current confirmation failed %+v %v", state, err)
			}
			drClaims(t, p, op.OperationID, 0, 0)
			riComplete(t, p)
		})
	}
}

func TestDualReleaseRejectsUnrequestedReceiptWithoutLosingClaims(t *testing.T) {
	ctx := context.Background()
	p := drPair(t)
	op, _ := riPrepare(t, p, 803, true)
	result, err := p.a.RecordResultFixture(ctx, eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "canceled_before_start"))
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, result)
	request, err := p.m.PrepareFinalize(ctx, p.b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := p.a.Finalize(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	original := mfEnvelope(t, response)
	bad, err := protocol.NewMessage("finalized", p.b.SessionID, riID(19001), json.RawMessage(original.Body))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.m.ReceiveFinalized(ctx, p.b, bad); err == nil {
		t.Fatal("unrequested receipt accepted")
	}
	drClaims(t, p, op.OperationID, 2, 0)
	if err = p.m.ReceiveFinalized(ctx, p.b, response); err != nil {
		t.Fatal(err)
	}
	drClaims(t, p, op.OperationID, 0, 0)
}

func TestDualReleaseResultBeforeStartedCanFinishThenReceiveRealStarted(t *testing.T) {
	ctx := context.Background()
	p := drPair(t)
	op, _ := riPrepare(t, p, 804, true)
	starts := 0
	op = riAuthorize(t, p, op, &starts)
	result, err := p.a.RecordResultFixture(ctx, eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""))
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, result)
	drFinalize(t, p, op.OperationID)
	// The first event never reached Manager before the second one was finalized.
	// It remains real evidence and must not cause a fabricated conflict.
	started, err := p.a.ReadEvent(ctx, op.OperationID, 1)
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, started)
	drClaims(t, p, op.OperationID, 0, 0)
	got, err := p.m.Get(ctx, op.OperationID)
	if err != nil || got.State != "succeeded" || got.ReleaseStatus != "released" || got.Blocked {
		t.Fatalf("late started overwrote release %+v %v", got, err)
	}
	cases, err := p.m.RecoveryCases(ctx, p.b.NodeID)
	if err != nil || len(cases) != 0 {
		t.Fatalf("normal reordering isolated node %v %v", cases, err)
	}
	scan := riScan(t, p, "pending", 10)
	if scan.Total != 0 {
		t.Fatalf("acknowledged finalized history still pending %+v", scan)
	}
	riComplete(t, p)
	if starts != 1 {
		t.Fatalf("late started repeated execution %d", starts)
	}
}

func TestDualReleaseKeepsManagerConflictUntilReceiptAndNeverDeletesNewClaims(t *testing.T) {
	ctx := context.Background()
	p := drPair(t)
	op, prepare := riPrepare(t, p, 806, true)
	result, err := p.a.RecordResultFixture(ctx, eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "canceled_before_start"))
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, result)
	var next map[string]any
	if json.Unmarshal(prepare, &next) != nil {
		t.Fatal("bad prepare")
	}
	next["message_id"] = riID(19801)
	next["body"].(map[string]any)["operation_id"] = riID(19802)
	nextWire := mfEncode(t, next)
	request, err := p.m.PrepareFinalize(ctx, p.b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := p.a.Finalize(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.m.AcceptFixture(ctx, p.b, mfUserID, nextWire); err == nil {
		t.Fatal("Agent release prematurely admitted Manager conflict")
	}
	drClaims(t, p, op.OperationID, 2, 0)
	if err = p.m.ReceiveFinalized(ctx, p.b, response); err != nil {
		t.Fatal(err)
	}
	newer, err := p.m.AcceptFixture(ctx, p.b, mfUserID, nextWire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.a.RecordReadyFixture(ctx, nextWire); err != nil {
		t.Fatal(err)
	}
	drClaims(t, p, newer.OperationID, 2, 2)
	replay, err := p.a.Finalize(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.m.ReceiveFinalized(ctx, p.b, replay); err != nil {
		t.Fatal(err)
	}
	drClaims(t, p, newer.OperationID, 2, 2)
	drClaims(t, p, op.OperationID, 0, 0)
}
