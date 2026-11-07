package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"docker-manager/internal/protocol"
)

const eventUser = "00000000-0000-4000-8000-000000000021"
const eventManager = "00000000-0000-4000-8000-000000000022"
const eventSession2 = "00000000-0000-4000-8000-000000000023"

func eventKeys() Keys {
	return Keys{DigestID: "manager-digest", EncryptionID: "manager-kek", DigestKey: bytes.Repeat([]byte{71}, 32), EncryptionKey: bytes.Repeat([]byte{72}, 32)}
}

func eventPrepare(t *testing.T, s *Store) (Binding, []byte, string) {
	t.Helper()
	raw, err := os.ReadFile("../../../contracts/examples/agent-v1.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var examples []map[string]any
	if err := json.Unmarshal(raw, &examples); err != nil {
		t.Fatal(err)
	}
	for _, e := range examples {
		if e["type"] != "prepare" {
			continue
		}
		body := e["body"].(map[string]any)
		payload, err := base64.StdEncoding.DecodeString(body["payload_b64"].(string))
		if err != nil {
			t.Fatal(err)
		}
		digest, err := s.InputDigest(payload)
		if err != nil {
			t.Fatal(err)
		}
		body["input_digest"] = digest
		b := Binding{NodeID: body["node_id"].(string), LedgerID: "00000000-0000-4000-8000-000000000005", SessionID: e["session_id"].(string)}
		out, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		return b, out, body["operation_id"].(string)
	}
	t.Fatal("prepare example absent")
	return Binding{}, nil, ""
}

func eventFixture(t *testing.T, authorized bool) (*Store, string, Binding, Operation) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := Create(ctx, path, eventManager, eventKeys())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	b, raw, id := eventPrepare(t, s)
	if err = s.CreateUserFixture(ctx, eventUser, true); err != nil {
		t.Fatal(err)
	}
	if err = s.BindSessionFixture(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"node.read", "config.read", "stack.deploy"} {
		if err = s.SetGrantFixture(ctx, eventUser, b.NodeID, c, true); err != nil {
			t.Fatal(err)
		}
	}
	op, err := s.AcceptFixture(ctx, b, eventUser, raw)
	if err != nil {
		t.Fatal(err)
	}
	if authorized {
		if err = s.RecordReadyFixture(ctx, b, id, op.Digest); err != nil {
			t.Fatal(err)
		}
		request, err := protocol.NewMessage("request_start", b.SessionID, "", map[string]any{"operation_id": id, "input_digest": op.Digest, "start_nonce": "00000000-0000-4000-8000-000000000024"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.AuthorizeStart(ctx, b, request); err != nil {
			t.Fatal(err)
		}
		op, err = s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	return s, path, b, op
}

func eventStarted(t *testing.T, s *Store, b Binding, op Operation) []byte {
	t.Helper()
	raw, err := protocol.NewMessage("started", b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "event_seq": 1, "phase": "START_COMMITTED", "permit_id": op.PermitID, "started_at": "2026-11-07T04:00:00.000Z"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func eventResult(t *testing.T, s *Store, b Binding, op Operation, outcome string, exit int, observation, ownership, code string) []byte {
	t.Helper()
	body := map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "event_seq": 2, "phase": "result", "outcome": outcome, "interrupt_reason": nil, "exit_code": nil, "error": nil, "observation": nil, "not_started": outcome == "not_started", "output_b64": nil, "output_truncated": false, "finished_at": "2026-11-07T04:01:00.000Z", "ownership_status": ownership}
	if outcome == "normal_exit" {
		body["exit_code"] = exit
		body["observation"] = map[string]any{"status": observation, "collected_at": "2026-11-07T04:01:00.000Z", "items": []any{}, "error": nil}
		if observation != "complete" {
			body["observation"].(map[string]any)["error"] = map[string]any{"category": "execution_failed", "code": "observation_failed", "message": "controlled fixture", "retry_action": "query_operation"}
		}
	}
	if outcome != "normal_exit" || exit != 0 {
		if code == "" {
			code = "fixture_result"
		}
		body["error"] = map[string]any{"category": "execution_failed", "code": code, "message": "controlled fixture", "retry_action": "query_operation"}
	}
	if outcome == "interrupted" {
		body["interrupt_reason"] = "canceled"
	}
	if outcome == "not_started" {
		body["event_seq"] = 1
		body["ownership_status"] = "not_checked"
	}
	raw, err := protocol.NewMessage("result", b.SessionID, "", body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.validator.Validate(raw); err != nil {
		t.Fatalf("invalid test fixture: %v", err)
	}
	return raw
}

func eventReceive(t *testing.T, s *Store, b Binding, raw []byte) {
	t.Helper()
	ack, err := s.ReceiveEvent(context.Background(), b, raw)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := s.validator.Validate(raw)
	out, err := s.validator.Validate(ack)
	if err != nil || out.Type != "ack" || out.SessionID == nil || *out.SessionID != b.SessionID || out.ReplyTo == nil || *out.ReplyTo != in.MessageID {
		t.Fatalf("invalid ack %v %s", err, ack)
	}
}

func TestManagerResultProjectionAndHeldClaims(t *testing.T) {
	cases := []struct {
		name, outcome, observation, ownership, code, state string
		exit                                               int
	}{
		{"success", "normal_exit", "complete", "consistent", "", "succeeded", 0},
		{"failed", "normal_exit", "complete", "consistent", "", "failed", 12},
		{"incomplete", "normal_exit", "error", "consistent", "", "reconciliation_required", 0},
		{"drift", "normal_exit", "complete", "drifted", "", "reconciliation_required", 0},
		{"unknown", "unknown", "", "unknown", "", "reconciliation_required", 0},
		{"interrupted", "interrupted", "", "unknown", "", "reconciliation_required", 0},
		{"not_started", "not_started", "", "not_checked", "preflight_failed", "failed", 0},
		{"canceled", "not_started", "", "not_checked", "canceled_before_start", "canceled", 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s, _, b, op := eventFixture(t, tt.outcome != "not_started")
			ctx := context.Background()
			if tt.outcome != "not_started" {
				eventReceive(t, s, b, eventStarted(t, s, b, op))
			}
			raw := eventResult(t, s, b, op, tt.outcome, tt.exit, tt.observation, tt.ownership, tt.code)
			eventReceive(t, s, b, raw)
			got, err := s.Get(ctx, op.OperationID)
			if err != nil || got.State != tt.state {
				t.Fatalf("projection %+v %v", got, err)
			}
			wantRelease := "pending"
			wantFinalize := 1
			if tt.state == "reconciliation_required" {
				wantRelease = "held"
				wantFinalize = 0
			}
			if got.ReleaseStatus != wantRelease {
				t.Fatal(got.ReleaseStatus)
			}
			claims, err := s.Claims(ctx, op.OperationID)
			if err != nil || len(claims) != 2 {
				t.Fatalf("claims=%+v %v", claims, err)
			}
			var n int
			if err = s.db.QueryRow("SELECT count(*) FROM outbox WHERE operation_id=? AND kind='finalize' AND delivery_state='pending' AND result_event_seq=? AND terminal_state=?", op.OperationID, got.LatestResultSeq, tt.state).Scan(&n); err != nil || n != wantFinalize {
				t.Fatalf("finalize=%d %v", n, err)
			}
		})
	}
}

func TestManagerOutOfOrderAndExactReplay(t *testing.T) {
	s, _, b, op := eventFixture(t, true)
	ctx := context.Background()
	raw := eventResult(t, s, b, op, "normal_exit", 0, "complete", "consistent", "")
	raw = bytes.Replace(raw, []byte(`"event_seq":2`), []byte(`"event_seq":2.0`), 1)
	raw = bytes.Replace(raw, []byte(`"phase":"result"`), []byte(`"phase" : "result"`), 1)
	eventReceive(t, s, b, raw)
	before, _ := s.Get(ctx, op.OperationID)
	eventReceive(t, s, b, eventStarted(t, s, b, op))
	after, _ := s.Get(ctx, op.OperationID)
	if after.State != "succeeded" || after.LatestResultSeq != 2 || after.MaxEventSeq != 2 || after.Revision != before.Revision+1 {
		t.Fatalf("late started regressed result: %+v", after)
	}
	env, _ := s.validator.Validate(raw)
	got, err := s.ReadEvent(ctx, op.OperationID, 2)
	if err != nil || !bytes.Equal(got.Body, env.Body) {
		t.Fatalf("body changed: %v", err)
	}
	var uses int
	_ = s.db.QueryRow("SELECT uses FROM keys WHERE singleton=1").Scan(&uses)
	for range 3 {
		eventReceive(t, s, b, raw)
	}
	replayed, _ := s.Get(ctx, op.OperationID)
	var afterUses int
	_ = s.db.QueryRow("SELECT uses FROM keys WHERE singleton=1").Scan(&afterUses)
	if afterUses != uses || replayed.Revision != after.Revision {
		t.Fatalf("duplicate mutated operation/uses: %d/%d %+v", uses, afterUses, replayed)
	}
}

func TestManagerEventConflictPersistsAndSuppressesFinalize(t *testing.T) {
	s, path, b, op := eventFixture(t, true)
	ctx := context.Background()
	raw := eventResult(t, s, b, op, "normal_exit", 0, "complete", "consistent", "")
	eventReceive(t, s, b, raw)
	conflict := bytes.Replace(raw, []byte(`04:01:00.000Z`), []byte(`04:01:01.000Z`), 1)
	ack, err := s.ReceiveEvent(ctx, b, conflict)
	if !errors.Is(err, ErrConflict) || len(ack) != 0 {
		t.Fatalf("conflict ack=%s err=%v", ack, err)
	}
	got, _ := s.Get(ctx, op.OperationID)
	if !got.Blocked || got.State != "reconciliation_required" || got.ReleaseStatus != "held" || got.LatestResultSeq != 2 {
		t.Fatalf("conflict overwritten %+v", got)
	}
	var frozen, n int
	_ = s.db.QueryRow("SELECT frozen FROM nodes WHERE id=?", b.NodeID).Scan(&frozen)
	_ = s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='finalize' AND delivery_state='suppressed'").Scan(&n)
	if frozen != 1 || n != 1 {
		t.Fatalf("isolation frozen=%d suppressed=%d", frozen, n)
	}
	for range 2 {
		if _, err = s.ReceiveEvent(ctx, b, conflict); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	_ = s.db.QueryRow("SELECT count(*) FROM event_conflicts").Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
	id := s.Identity()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path, id, eventKeys())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	b.SessionID = eventSession2
	if err = s2.BindSessionFixture(ctx, b); err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(`"session_id":"00000000-0000-4000-8000-000000000006"`), []byte(`"session_id":"`+b.SessionID+`"`))
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(raw, &wire)
	wire["session_id"], _ = json.Marshal(b.SessionID)
	raw, _ = json.Marshal(wire)
	eventReceive(t, s2, b, raw)
	got, _ = s2.Get(ctx, op.OperationID)
	if !got.Blocked {
		t.Fatal("lost blocked")
	}
}

func TestManagerInvalidEventsHaveNoAck(t *testing.T) {
	for _, mode := range []string{"session", "ledger", "node", "digest", "permit", "future", "notstarted_after_started"} {
		t.Run(mode, func(t *testing.T) {
			s, _, b, op := eventFixture(t, true)
			ctx := context.Background()
			raw := eventStarted(t, s, b, op)
			switch mode {
			case "session":
				b.SessionID = eventSession2
			case "ledger":
				b.LedgerID = eventSession2
			case "node":
				b.NodeID = eventSession2
			case "digest":
				raw = bytes.ReplaceAll(raw, []byte(op.Digest), []byte(strings.Repeat("A", 43)))
			case "permit":
				raw = bytes.ReplaceAll(raw, []byte(op.PermitID), []byte(eventSession2))
			case "future":
				raw = bytes.Replace(raw, []byte(`"event_seq":1`), []byte(`"event_seq":3`), 1)
			case "notstarted_after_started":
				eventReceive(t, s, b, raw)
				raw = eventResult(t, s, b, op, "not_started", 0, "", "not_checked", "preflight_failed")
			}
			ack, err := s.ReceiveEvent(ctx, b, raw)
			if err == nil || len(ack) != 0 {
				t.Fatalf("invalid accepted %s %v", ack, err)
			}
		})
	}
}

func TestManagerConcurrentResultReplay(t *testing.T) {
	s, _, b, op := eventFixture(t, true)
	raw := eventResult(t, s, b, op, "normal_exit", 0, "complete", "consistent", "")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.ReceiveEvent(context.Background(), b, raw); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	_ = s.db.QueryRow("SELECT count(*) FROM operation_events").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	got, _ := s.Get(context.Background(), op.OperationID)
	if got.Revision != op.Revision+1 {
		t.Fatal(got.Revision)
	}
}

func TestManagerCancelAfterPermitDoesNotChangeActualSuccess(t *testing.T) {
	s, _, b, op := eventFixture(t, true)
	if err := s.RequestCancelFixture(context.Background(), op.OperationID); err != nil {
		t.Fatal(err)
	}
	eventReceive(t, s, b, eventResult(t, s, b, op, "normal_exit", 0, "complete", "consistent", ""))
	got, _ := s.Get(context.Background(), op.OperationID)
	if got.State != "succeeded" || !got.CancelRequested {
		t.Fatalf("invented cancel %+v", got)
	}
	var state string
	if err := s.db.QueryRow("SELECT delivery_state FROM outbox WHERE operation_id=? AND kind='cancel'", op.OperationID).Scan(&state); err != nil || state != "confirmed" {
		t.Fatalf("result did not confirm cancel: %q %v", state, err)
	}
}

func TestManagerLaterExecutionContradictsNotStarted(t *testing.T) {
	s, _, b, op := eventFixture(t, false)
	ctx := context.Background()
	eventReceive(t, s, b, eventResult(t, s, b, op, "not_started", 0, "", "not_checked", "preflight_failed"))
	ack, err := s.ReceiveEvent(ctx, b, eventResult(t, s, b, op, "normal_exit", 0, "complete", "consistent", ""))
	if !errors.Is(err, ErrConflict) || len(ack) != 0 {
		t.Fatalf("contradiction ack=%s error=%v", ack, err)
	}
	got, err := s.Get(ctx, op.OperationID)
	if err != nil || !got.Blocked || got.ReleaseStatus != "held" || got.LatestResultSeq != 1 {
		t.Fatalf("contradiction not isolated %+v %v", got, err)
	}
	var state string
	_ = s.db.QueryRow("SELECT delivery_state FROM outbox WHERE operation_id=? AND kind='finalize'", op.OperationID).Scan(&state)
	if state != "suppressed" {
		t.Fatal(state)
	}
}

func TestManagerEventRejectsMissingPrepareEvidence(t *testing.T) {
	s, _, b, op := eventFixture(t, true)
	if _, err := s.db.Exec("DELETE FROM outbox WHERE operation_id=? AND kind='prepare'", op.OperationID); err != nil {
		t.Fatal(err)
	}
	ack, err := s.ReceiveEvent(context.Background(), b, eventStarted(t, s, b, op))
	if !errors.Is(err, ErrEvidence) || len(ack) != 0 {
		t.Fatalf("missing intent acknowledged: %s %v", ack, err)
	}
}
