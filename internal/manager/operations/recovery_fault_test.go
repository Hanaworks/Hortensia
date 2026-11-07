package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"reflect"
	"testing"
)

// Real SQLite statement errors and deferred foreign-key errors at COMMIT.
// TEMP objects disappear on close and do not weaken persisted DDL validation.
func riInstallFault(t *testing.T, s *Store, target string, commit bool) func() {
	t.Helper()
	clause := "AFTER INSERT ON main." + target
	if target == "completed" {
		clause = "AFTER UPDATE OF frozen ON main.nodes WHEN OLD.frozen=1 AND NEW.frozen=0"
	}
	action := "SELECT RAISE(ABORT,'private-manager-fault-secret');"
	if commit {
		if _, err := s.db.Exec(`CREATE TEMP TABLE ri_fault_parent(id TEXT PRIMARY KEY);
CREATE TEMP TABLE ri_fault_child(parent TEXT REFERENCES ri_fault_parent(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
			t.Fatal(err)
		}
		action = "INSERT INTO ri_fault_child(parent) VALUES('private-manager-fault-secret');"
	}
	if _, err := s.db.Exec("CREATE TEMP TRIGGER ri_fault " + clause + " BEGIN " + action + " END"); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := s.db.Exec("DROP TRIGGER ri_fault"); err != nil {
			t.Fatal(err)
		}
		if commit {
			if mfCount(t, s, "temp.ri_fault_child") != 0 {
				t.Fatal("failed commit left partial trigger row")
			}
			if _, err := s.db.Exec("DROP TABLE ri_fault_child; DROP TABLE ri_fault_parent"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRecoveryFaultPageStatementAndCommitAreAtomic(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "statement"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			p := riOpenPair(t)
			op, _ := riPrepare(t, p, 70, true)
			previousPages := mfCount(t, p.m, "recovery_pages")
			request, err := p.m.StartRecovery(context.Background(), p.b, "all", 1)
			if err != nil {
				t.Fatal(err)
			}
			response, err := p.a.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			before, err := p.m.ReadRecoveryState(context.Background(), p.b.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			uses := mfUses(t, p.m)
			remove := riInstallFault(t, p.m, "recovery_pages", commit)
			_, err = p.m.ReceiveRecoveryPage(context.Background(), p.b, response)
			mfStorageFailure(t, err)
			after, err := p.m.ReadRecoveryState(context.Background(), p.b.NodeID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed page changed progress: before=%+v after=%+v error=%v", before, after, err)
			}
			if mfCount(t, p.m, "recovery_pages") != previousPages || mfCount(t, p.m, "recovery_members") != 0 {
				t.Fatal("failed page left durable page/member")
			}
			var accepted int
			if err := p.m.db.QueryRow("SELECT accepted FROM recovery_requests WHERE message_id=?", mfEnvelope(t, request).MessageID).Scan(&accepted); err != nil || accepted != 0 {
				t.Fatalf("failed page accepted request: %d %v", accepted, err)
			}
			if mfUses(t, p.m) != uses+1 {
				t.Fatal("page failure refunded wrapping reservation")
			}
			riFrozen(t, p.m, p.b.NodeID, true)
			remove()
			state, err := p.m.ReceiveRecoveryPage(context.Background(), p.b, response)
			if err != nil || state.Pages != 1 || state.Total != 1 {
				t.Fatalf("page retry: %+v %v", state, err)
			}
			body, err := p.m.ReadRecoveryPage(context.Background(), state.RoundID, 0)
			if err != nil || !bytes.Equal(body, mfEnvelope(t, response).Body) {
				t.Fatalf("retry lost exact page: %v", err)
			}
			if mfUses(t, p.m) != uses+2 {
				t.Fatal("retry reused burned wrapping reservation")
			}
			if _, err := p.m.ReceiveRecoveryPage(context.Background(), p.b, response); err != nil {
				t.Fatal(err)
			}
			if mfUses(t, p.m) != uses+2 || mfCount(t, p.m, "recovery_pages") != previousPages+1 || mfCount(t, p.m, "recovery_members") != 1 {
				t.Fatal("duplicate page consumed quota or added rows")
			}
			riAssertWaiting(t, p, op.OperationID)
		})
	}
}

func TestRecoveryFaultCaseStatementAndCommitNeverAckPartialEvidence(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "statement"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			p := riNewPair(t, t.TempDir())
			unknown := Operation{OperationID: riID(9100), Digest: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x66}, 32))}
			wire := eventResult(t, p.m, p.b, unknown, "not_started", 0, "", "not_checked", "canceled_before_start")
			uses := mfUses(t, p.m)
			remove := riInstallFault(t, p.m, "recovery_cases", commit)
			ack, err := p.m.ReceiveRecoveryEvent(context.Background(), p.b, wire)
			mfStorageFailure(t, err)
			if len(ack) != 0 || mfCount(t, p.m, "recovery_cases") != 0 || mfCount(t, p.m, "operations") != 0 {
				t.Fatal("failed case acknowledged or fabricated evidence")
			}
			if mfUses(t, p.m) != uses+1 {
				t.Fatal("case failure refunded wrapping reservation")
			}
			riFrozen(t, p.m, p.b.NodeID, true)
			remove()
			ack, err = p.m.ReceiveRecoveryEvent(context.Background(), p.b, wire)
			if err != nil || len(ack) == 0 {
				t.Fatalf("case retry: %v", err)
			}
			in, out := mfEnvelope(t, wire), mfEnvelope(t, ack)
			if out.Type != "ack" || out.ReplyTo == nil || *out.ReplyTo != in.MessageID {
				t.Fatal("case ack lost current event correlation")
			}
			cases, err := p.m.RecoveryCases(context.Background(), p.b.NodeID)
			if err != nil || len(cases) != 1 || cases[0].OperationID != unknown.OperationID {
				t.Fatalf("case absent after successful ack: %+v %v", cases, err)
			}
			evidence, err := p.m.ReadRecoveryCase(context.Background(), cases[0].ID)
			if err != nil || !bytes.Equal(evidence, in.Body) {
				t.Fatalf("case original evidence missing: %v", err)
			}
			if mfUses(t, p.m) != uses+2 {
				t.Fatal("case retry did not reserve fresh wrapping use")
			}
			if _, err := p.m.ReceiveRecoveryEvent(context.Background(), p.b, wire); err != nil {
				t.Fatal(err)
			}
			if mfUses(t, p.m) != uses+2 || mfCount(t, p.m, "recovery_cases") != 1 {
				t.Fatal("case duplicate added row or consumed quota")
			}
		})
	}
}

func TestRecoveryFaultAbsenceStatementAndCommitKeepClaimsAndOriginalState(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "statement"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			p := riOpenPair(t)
			op, _ := riPrepare(t, p, 10, false)
			beforeIntents, err := p.m.Intents(context.Background(), op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			scan := riScan(t, p, "all", 1)
			remove := riInstallFault(t, p.m, "absence_releases", commit)
			_, err = p.m.ProcessRecovery(context.Background(), p.b)
			mfStorageFailure(t, err)
			after, err := p.m.Get(context.Background(), op.OperationID)
			if err != nil || !reflect.DeepEqual(op, after) {
				t.Fatalf("partial absence projection: before=%+v after=%+v %v", op, after, err)
			}
			intents, err := p.m.Intents(context.Background(), op.OperationID)
			if err != nil || !reflect.DeepEqual(beforeIntents, intents) {
				t.Fatalf("partial absence intents: %+v %v", intents, err)
			}
			claims, err := p.m.Claims(context.Background(), op.OperationID)
			if err != nil || len(claims) != 2 || mfCount(t, p.m, "absence_releases") != 0 {
				t.Fatal("failed absence released claims or wrote proof")
			}
			if mfCount(t, p.m, "operation_events") != 0 {
				t.Fatal("failed absence fabricated result")
			}
			riFrozen(t, p.m, p.b.NodeID, true)
			remove()
			state, err := p.m.ProcessRecovery(context.Background(), p.b)
			if err != nil || state.Status != "ready" {
				t.Fatalf("absence retry: %+v %v", state, err)
			}
			riAssertAbsenceReleased(t, p.m, op.OperationID, scan.RoundID)
			if _, err := p.m.ProcessRecovery(context.Background(), p.b); err != nil {
				t.Fatal(err)
			}
			if mfCount(t, p.m, "absence_releases") != 1 {
				t.Fatal("absence retry duplicated proof")
			}
			riComplete(t, p)
		})
	}
}

func TestRecoveryFaultCompletedStatementAndCommitNeverThaw(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "statement"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			p := riNewPair(t, t.TempDir())
			riScan(t, p, "all", 1)
			request, response := riPrepareComplete(t, p)
			before, err := p.m.ReadRecoveryState(context.Background(), p.b.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			remove := riInstallFault(t, p.m, "completed", commit)
			_, err = p.m.ReceiveRecoveryCompleted(context.Background(), p.b, response)
			mfStorageFailure(t, err)
			after, err := p.m.ReadRecoveryState(context.Background(), p.b.NodeID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed completion left partial round: %+v %+v %v", before, after, err)
			}
			var accepted int
			if err := p.m.db.QueryRow("SELECT accepted FROM recovery_requests WHERE message_id=?", mfEnvelope(t, request).MessageID).Scan(&accepted); err != nil || accepted != 0 {
				t.Fatalf("failed completion accepted receipt: %d %v", accepted, err)
			}
			riFrozen(t, p.m, p.b.NodeID, true)
			remove()
			// Agent already committed its side; the same durable request can be
			// replayed without a second execution or invented completion.
			replayed, err := p.a.CompleteReconcile(context.Background(), request)
			if err != nil || !bytes.Equal(mfEnvelope(t, replayed).Body, mfEnvelope(t, response).Body) {
				t.Fatalf("Agent completion replay: %v", err)
			}
			state, err := p.m.ReceiveRecoveryCompleted(context.Background(), p.b, replayed)
			if err != nil || state.Status != "completed" {
				t.Fatalf("completed retry: %+v %v", state, err)
			}
			riFrozen(t, p.m, p.b.NodeID, false)
			if mfCount(t, p.m, "operations") != 0 || mfCount(t, p.m, "resource_claims") != 0 {
				t.Fatal("empty completion fabricated business state")
			}
		})
	}
}
