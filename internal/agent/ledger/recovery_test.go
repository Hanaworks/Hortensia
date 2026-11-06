package ledger

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

var recoveryKEK = JournalKey{ID: "recovery-test-kek", Key: bytes.Repeat([]byte{0x71}, 32)}

func recoveryFixtureStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := CreateJournal(context.Background(), path, crashNode, "recovery-ledger-key", crashKey, recoveryKEK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func recoveryResultRaw(t *testing.T, r Record, seq uint64, outcome string) []byte {
	t.Helper()
	e := map[string]any{"category": "reconciliation_required", "code": "result_unknown", "retry_action": "query_operation", "message": "controlled-secret-result"}
	b := map[string]any{"operation_id": r.OperationID, "input_digest": r.Digest, "event_seq": seq, "phase": "result", "outcome": outcome,
		"interrupt_reason": nil, "exit_code": nil, "error": e, "observation": nil, "not_started": false, "output_b64": base64.StdEncoding.EncodeToString([]byte("controlled-secret-output")), "output_truncated": false, "finished_at": "2026-11-07T01:00:00.000Z", "ownership_status": "unknown"}
	switch outcome {
	case "not_started":
		b["not_started"], b["output_b64"], b["ownership_status"] = true, nil, "not_checked"
		e["category"], e["code"], e["retry_action"] = "execution_failed", "canceled_before_start", "none"
	case "normal_exit":
		b["exit_code"], b["error"], b["ownership_status"] = 0, nil, "consistent"
		b["observation"] = map[string]any{"status": "complete", "collected_at": "2026-11-07T01:00:00.000Z", "items": []any{}, "error": nil}
	case "interrupted":
		b["interrupt_reason"], b["exit_code"] = "canceled", 0
	}
	raw, err := protocol.NewMessage("result", r.PreparedSession, "", b)
	if err != nil {
		t.Fatal(err)
	}
	v, err := protocol.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(raw); err != nil {
		t.Fatalf("invalid result fixture: %v", err)
	}
	return raw
}

func recoveryDecode(t *testing.T, s *Store, operation string) map[string]any {
	t.Helper()
	raw, err := s.RecoveryRecord(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	// Validate the actual projection inside the existing reconcile_reply shape.
	examples, err := os.ReadFile("../../../contracts/examples/agent-v1.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	if err := json.Unmarshal(examples, &messages); err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, message := range messages {
		if message["type"] != "reconcile_reply" {
			continue
		}
		body := message["body"].(map[string]any)
		body["records"], body["total"], body["complete"], body["next_cursor"], body["error"] = []any{view}, 1, true, nil, nil
		body["ledger_id"] = s.identity.LedgerID
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.validator.Validate(encoded); err != nil {
			t.Fatalf("recovery projection violates existing schema: %v", err)
		}
		checked = true
		break
	}
	if !checked {
		t.Fatal("reconcile_reply fixture not found")
	}
	if view["claims_held"] != true || view["final_state"] != nil || view["finalized_result_event_seq"] != nil {
		t.Fatalf("unexpected release projection: %s", raw)
	}
	return view
}

func TestJournalRecoveryStagesAndAcknowledgement(t *testing.T) {
	for _, stage := range []string{"ready", "started", "not_started", "normal_exit", "interrupted", "unknown"} {
		t.Run(stage, func(t *testing.T) {
			s, path := recoveryFixtureStore(t)
			r, q, p := gateReady(t, s)
			wantSeq := uint64(0)
			if stage != "ready" && stage != "not_started" {
				g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {})
				if err != nil {
					t.Fatal(err)
				}
				if err := g.Authorize(p); err != nil {
					t.Fatal(err)
				}
				wantSeq = 1
			}
			var result Event
			if stage != "ready" && stage != "started" {
				wantSeq++
				var err error
				result, err = s.RecordResultFixture(context.Background(), recoveryResultRaw(t, r, wantSeq, stage))
				if err != nil {
					t.Fatal(err)
				}
			}
			view := recoveryDecode(t, s, r.OperationID)
			wantPhase := "result"
			if stage == "ready" {
				wantPhase = PhaseReady
			}
			if stage == "started" {
				wantPhase = PhaseStartCommitted
			}
			if view["phase"] != wantPhase || view["last_event_seq"] != float64(wantSeq) || view["report_acked"] != false {
				t.Fatalf("unexpected view: %+v", view)
			}
			if result.Seq != 0 {
				latest := view["latest_result"].(map[string]any)
				if latest["event_seq"] != float64(result.Seq) || latest["operation_id"] != r.OperationID || latest["input_digest"] != r.Digest || latest["outcome"] != stage {
					t.Fatalf("inconsistent result: %+v", view)
				}
				if err := s.AcknowledgeEvent(context.Background(), r.OperationID, r.Digest, result.Seq, r.PreparedSession); err != nil {
					t.Fatal(err)
				}
				if recoveryDecode(t, s, r.OperationID)["report_acked"] != true {
					t.Fatal("latest result acknowledgement lost")
				}
			}
			identity := s.Identity()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenJournal(context.Background(), path, identity, crashKey, recoveryKEK)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			view = recoveryDecode(t, reopened, r.OperationID)
			if stage == "started" {
				wantPhase = "reconciliation_required"
			}
			if view["phase"] != wantPhase {
				t.Fatalf("old boot projection: %+v", view)
			}
			if _, err := reopened.NewGate(context.Background(), q, time.Now, func(startgate.Permit) { t.Error("restarted old operation") }); !errors.Is(err, ErrFrozen) {
				t.Fatalf("old gate: %v", err)
			}
			if stage == "started" {
				if _, err := reopened.RecordResultFixture(context.Background(), recoveryResultRaw(t, r, 2, "normal_exit")); !errors.Is(err, ErrFrozen) {
					t.Fatalf("old boot fabricated result: %v", err)
				}
			}
		})
	}
}

func TestJournalRecoveryRejectsIncompleteOrAlteredEvidence(t *testing.T) {
	for _, alteration := range []string{"claim", "state", "cipher"} {
		t.Run(alteration, func(t *testing.T) {
			s, _ := recoveryFixtureStore(t)
			r, _, _ := gateReady(t, s)
			if _, err := s.RecordResultFixture(context.Background(), recoveryResultRaw(t, r, 1, "not_started")); err != nil {
				t.Fatal(err)
			}
			statement := "DELETE FROM local_claims WHERE resource_kind='stack'"
			if alteration == "state" {
				statement = "UPDATE event_state SET last_seq=2"
			}
			if alteration == "cipher" {
				statement = "UPDATE execution_events SET body_cipher=randomblob(length(body_cipher))"
			}
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if raw, err := s.RecoveryRecord(context.Background(), r.OperationID); err == nil || len(raw) != 0 {
				t.Fatalf("bad evidence returned: %s, %v", raw, err)
			}
		})
	}
}

func TestJournalRecoveryConflictRemainsIsolated(t *testing.T) {
	s, _ := recoveryFixtureStore(t)
	r, _, _ := gateReady(t, s)
	raw := recoveryResultRaw(t, r, 1, "not_started")
	if _, err := s.RecordResultFixture(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(raw, []byte("controlled-secret-result"), []byte("changed-secret-result"), 1)
	if bytes.Equal(changed, raw) {
		t.Fatal("conflict fixture unchanged")
	}
	if _, err := s.RecordResultFixture(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	view := recoveryDecode(t, s, r.OperationID)
	if view["phase"] != "reconciliation_required" || view["last_event_seq"] != float64(1) || view["latest_result"] == nil {
		t.Fatalf("lost isolation/evidence: %+v", view)
	}
}

func TestJournalRecoveryPreservesEmbeddedResultBytes(t *testing.T) {
	s, _ := recoveryFixtureStore(t)
	r, _, _ := gateReady(t, s)
	raw := recoveryResultRaw(t, r, 1, "not_started")
	raw = bytes.Replace(raw, []byte("controlled-secret-result"), []byte(strings.Repeat("<", 500)), 1)
	raw = bytes.Replace(raw, []byte(`"phase":"result"`), []byte(`"phase" : "result"`), 1)
	event, err := s.RecordResultFixture(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := s.RecoveryRecord(context.Background(), r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Latest json.RawMessage `json:"latest_result"`
	}
	if err := json.Unmarshal(projection, &view); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(view.Latest, event.Body) {
		t.Fatal("recovery re-encoded or expanded the persisted result body")
	}
	if len(projection) > len(event.Body)+1024 {
		t.Fatal("recovery framing expanded the body beyond its fixed metadata budget")
	}
}
