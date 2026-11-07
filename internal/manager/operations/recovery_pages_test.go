package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"docker-manager/internal/protocol"
)

const recoveryScanID = "00000000-0000-4000-8000-000000000099"

func recoveryPageRecord(id string) map[string]any {
	return map[string]any{"operation_id": id, "input_digest": base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "stack_id": id, "version_id": id, "project_name": "page-secret", "phase": "ready", "last_event_seq": 0, "result_event_seq": nil, "latest_result": nil, "claims_held": true, "report_acked": false, "final_state": nil, "finalized_result_event_seq": nil}
}
func recoveryPageReply(t *testing.T, s *Store, b Binding, request []byte, total int, records []any, next any) []byte {
	t.Helper()
	q, err := s.validator.Validate(request)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Scope string `json:"scope"`
	}
	json.Unmarshal(q.Body, &body)
	raw, err := protocol.NewMessage("reconcile_reply", b.SessionID, q.MessageID, map[string]any{"scope": body.Scope, "scan_id": recoveryScanID, "ledger_id": b.LedgerID, "total": total, "records": records, "next_cursor": next, "complete": next == nil, "error": nil})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestRecoveryPagesEncryptedReplayOrderingAndReadIntegrity(t *testing.T) {
	s, b, _ := recoveryStoreFixture(t)
	ctx := context.Background()
	q, err := s.StartRecovery(ctx, b, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	p := recoveryPageReply(t, s, b, q, 2, []any{recoveryPageRecord("00000000-0000-4000-8000-000000000010")}, "next")
	st, err := s.ReceiveRecoveryPage(ctx, b, p)
	if err != nil || st.Pages != 1 || st.Seen != 1 || st.Total != 2 {
		t.Fatalf("page %+v %v", st, err)
	}
	env, _ := s.validator.Validate(p)
	raw, err := s.ReadRecoveryPage(ctx, st.RoundID, 0)
	if err != nil || !bytes.Equal(raw, env.Body) {
		t.Fatalf("original body changed: %v", err)
	}
	var uses int
	if err = s.db.QueryRow(`SELECT uses FROM keys`).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReceiveRecoveryPage(ctx, b, p); err != nil {
		t.Fatal(err)
	}
	var after int
	s.db.QueryRow(`SELECT uses FROM keys`).Scan(&after)
	if after != uses {
		t.Fatal("duplicate consumed encryption use")
	}
	q2, err := s.NextRecoveryRequest(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.NextRecoveryRequest(ctx, b)
	if err != nil || !bytes.Equal(q2, replay) {
		t.Fatalf("next request changed: %v", err)
	}
	for _, bad := range []struct {
		total int
		id    string
		next  any
	}{{3, "00000000-0000-4000-8000-000000000011", nil}, {2, "00000000-0000-4000-8000-000000000010", nil}, {2, "00000000-0000-4000-8000-000000000011", "next"}} {
		if _, err = s.ReceiveRecoveryPage(ctx, b, recoveryPageReply(t, s, b, q2, bad.total, []any{recoveryPageRecord(bad.id)}, bad.next)); err == nil {
			t.Fatal("bad fixed page accepted")
		}
	}
	st, err = s.ReceiveRecoveryPage(ctx, b, recoveryPageReply(t, s, b, q2, 2, []any{recoveryPageRecord("00000000-0000-4000-8000-000000000011")}, nil))
	if err != nil || st.Status != "enumerated" {
		t.Fatalf("complete %+v %v", st, err)
	}
	if _, err = s.db.Exec(`UPDATE recovery_members SET record_no=1 WHERE round_id=? AND ordinal=0`, st.RoundID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadRecoveryPage(ctx, st.RoundID, 0); !errors.Is(err, ErrEvidence) {
		t.Fatalf("corrupt member accepted: %v", err)
	}
}
func TestRecoveryPagesEmptyAndRequestBinding(t *testing.T) {
	s, b, _ := recoveryStoreFixture(t)
	ctx := context.Background()
	q, err := s.StartRecovery(ctx, b, "pending", 2)
	if err != nil {
		t.Fatal(err)
	}
	p := recoveryPageReply(t, s, b, q, 0, []any{}, nil)
	var obj map[string]any
	json.Unmarshal(p, &obj)
	obj["reply_to"] = recoveryScanID
	bad, _ := json.Marshal(obj)
	if _, err = s.ReceiveRecoveryPage(ctx, b, bad); err == nil {
		t.Fatal("unrequested reply accepted")
	}
	st, err := s.ReceiveRecoveryPage(ctx, b, p)
	if err != nil || st.Pages != 1 || st.Total != 0 || st.Status != "enumerated" {
		t.Fatalf("empty %+v %v", st, err)
	}
	if err = s.requireBinding(ctx, s.db, b, false); !errors.Is(err, ErrFrozen) {
		t.Fatal("page completion opened admission")
	}
}

func TestRecoveryPagesBudgetAndFailureRestart(t *testing.T) {
	ctx := context.Background()
	s, b, _ := recoveryStoreFixture(t)
	q, err := s.StartRecovery(ctx, b, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][]byte{
		recoveryPageReply(t, s, b, q, 4097, []any{}, nil),
		recoveryPageReply(t, s, b, q, 2, []any{recoveryPageRecord("00000000-0000-4000-8000-000000000010"), recoveryPageRecord("00000000-0000-4000-8000-000000000011")}, nil),
		recoveryPageReply(t, s, b, q, 1, []any{}, "next"),
	} {
		if _, err = s.ReceiveRecoveryPage(ctx, b, p); err == nil {
			t.Fatal("budget/nonprogress accepted")
		}
	}
	var uses int
	s.db.QueryRow(`SELECT uses FROM keys`).Scan(&uses)
	if uses != 0 {
		t.Fatal("precheck failure used key")
	}
	env, _ := s.validator.Validate(q)
	busy, err := s.RecordRecoveryFailureFixture(ctx, b, env.MessageID, "busy")
	if err != nil || busy.Status != "collecting" {
		t.Fatalf("busy %+v %v", busy, err)
	}
	expired, err := s.RecordRecoveryFailureFixture(ctx, b, env.MessageID, "scan_expired")
	if err != nil || expired.Status != "expired" {
		t.Fatalf("expired %+v %v", expired, err)
	}
	next, err := s.StartRecovery(ctx, b, "all", 1)
	if err != nil || bytes.Equal(q, next) {
		t.Fatalf("restart %v", err)
	}
	if _, err = s.ReceiveRecoveryPage(ctx, b, recoveryPageReply(t, s, b, q, 0, []any{}, nil)); err == nil {
		t.Fatal("old request accepted into new round")
	}
	st, err := s.ReceiveRecoveryPage(ctx, b, recoveryPageReply(t, s, b, next, 0, []any{}, nil))
	if err != nil || st.Status != "enumerated" {
		t.Fatalf("new empty %v", err)
	}
}
func TestRecoveryPagesCipherRequestsAndMembersCannotBeReplaced(t *testing.T) {
	for _, mutation := range []string{"cipher", "request", "member", "missing-member", "counter"} {
		t.Run(mutation, func(t *testing.T) {
			s, b, _ := recoveryStoreFixture(t)
			ctx := context.Background()
			q, err := s.StartRecovery(ctx, b, "all", 1)
			if err != nil {
				t.Fatal(err)
			}
			st, err := s.ReceiveRecoveryPage(ctx, b, recoveryPageReply(t, s, b, q, 1, []any{recoveryPageRecord("00000000-0000-4000-8000-000000000010")}, nil))
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "cipher":
				_, err = s.db.Exec(`UPDATE recovery_pages SET body_cipher=randomblob(length(body_cipher)) WHERE round_id=?`, st.RoundID)
			case "request":
				_, err = s.db.Exec(`UPDATE recovery_requests SET cursor='bad' WHERE round_id=?`, st.RoundID)
			case "member":
				_, err = s.db.Exec(`UPDATE recovery_members SET operation_id=? WHERE round_id=?`, recoveryScanID, st.RoundID)
			case "missing-member":
				_, err = s.db.Exec(`DELETE FROM recovery_members WHERE round_id=?`, st.RoundID)
			case "counter":
				_, err = s.db.Exec(`UPDATE recovery_scans SET body_bytes=body_bytes+1 WHERE id=?`, st.RoundID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if raw, err := s.ReadRecoveryPage(ctx, st.RoundID, 0); err == nil || len(raw) != 0 {
				t.Fatalf("corrupt evidence returned: %v", err)
			}
		})
	}
}
func TestRecoveryPagesReplayAfterKeyExhaustionAndConflictCase(t *testing.T) {
	s, b, _ := recoveryStoreFixture(t)
	ctx := context.Background()
	q, err := s.StartRecovery(ctx, b, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	p := recoveryPageReply(t, s, b, q, 1, []any{recoveryPageRecord("00000000-0000-4000-8000-000000000010")}, nil)
	st, err := s.ReceiveRecoveryPage(ctx, b, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE keys SET uses=?`, maxKeyUses); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReceiveRecoveryPage(ctx, b, p); err != nil {
		t.Fatalf("saved replay requires new key use: %v", err)
	}
	if _, err = s.ReadRecoveryPage(ctx, st.RoundID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE keys SET uses=?`, maxKeyUses-1); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(p, []byte("page-secret"), []byte("other-value"), 1)
	if _, err = s.ReceiveRecoveryPage(ctx, b, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict %v", err)
	}
	var cases, pages int
	s.db.QueryRow(`SELECT count(*) FROM recovery_cases WHERE kind='page_conflict'`).Scan(&cases)
	s.db.QueryRow(`SELECT count(*) FROM recovery_pages`).Scan(&pages)
	if cases != 1 || pages != 1 {
		t.Fatalf("conflict case=%d pages=%d", cases, pages)
	}
	body, _ := s.ReadRecoveryPage(ctx, st.RoundID, 0)
	env, _ := s.validator.Validate(p)
	if !bytes.Equal(body, env.Body) {
		t.Fatal("conflict replaced original body")
	}
}

func TestRecoveryPagesErrorReplyKeepsScopeAndScanBinding(t *testing.T) {
	ctx := context.Background()
	s, b, _ := recoveryStoreFixture(t)
	q, err := s.StartRecovery(ctx, b, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := s.validator.Validate(q)
	for _, fields := range []struct {
		scope string
		scan  any
	}{{"pending", nil}, {"all", recoveryScanID}} {
		raw, err := protocol.NewMessage("reconcile_reply", b.SessionID, env.MessageID, map[string]any{"scope": fields.scope, "scan_id": fields.scan, "ledger_id": b.LedgerID, "total": 0, "records": []any{}, "next_cursor": nil, "complete": false, "error": map[string]any{"code": "busy", "message": "try later", "category": "unavailable", "retry_action": "same_request"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.validator.Validate(raw); err != nil {
			t.Fatalf("test must use valid schema: %v", err)
		}
		if _, err = s.ReceiveRecoveryPage(ctx, b, raw); err == nil {
			t.Fatal("mismatched error page was accepted")
		}
	}
	state, err := s.ReadRecoveryState(ctx, b.NodeID)
	if err != nil || state.Status != "collecting" {
		t.Fatalf("invalid error affected round %+v %v", state, err)
	}
}

func TestRecoveryPagesFullEnvelopeAndAccumulatedBodyBudgets(t *testing.T) {
	ctx := context.Background()
	s, b, _ := recoveryStoreFixture(t)
	q, err := s.StartRecovery(ctx, b, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	oversized := bytes.Repeat([]byte(" "), (8<<20)+1)
	if _, err = s.ReceiveRecoveryPage(ctx, b, oversized); !errors.Is(err, ErrInput) {
		t.Fatalf("oversized envelope: %v", err)
	}
	padding := bytes.Repeat([]byte(" "), 7<<20)
	for i := 0; i < 5; i++ {
		id := []string{"00000000-0000-4000-8000-000000000010", "00000000-0000-4000-8000-000000000011", "00000000-0000-4000-8000-000000000012", "00000000-0000-4000-8000-000000000013", "00000000-0000-4000-8000-000000000014"}[i]
		var next any
		if i < 4 {
			next = []string{"cursor1", "cursor2", "cursor3", "cursor4"}[i]
		}
		raw := recoveryPageReply(t, s, b, q, 5, []any{recoveryPageRecord(id)}, next)
		env, _ := s.validator.Validate(raw)
		head, _ := json.Marshal(map[string]any{"protocol": 1, "type": "reconcile_reply", "message_id": env.MessageID, "session_id": b.SessionID, "reply_to": *env.ReplyTo})
		wire := append(head[:len(head)-1], []byte(`,"body":{`)...)
		wire = append(wire, padding...)
		wire = append(wire, env.Body[1:]...)
		wire = append(wire, '}')
		state, e := s.ReceiveRecoveryPage(ctx, b, wire)
		if i == 4 {
			if !errors.Is(e, ErrRecovery) {
				t.Fatalf("aggregate overflow accepted: %+v %v", state, e)
			}
			break
		}
		if e != nil || state.Seen != i+1 {
			t.Fatalf("legal padded page %d: %+v %v", i, state, e)
		}
		q, err = s.NextRecoveryRequest(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := s.ReadRecoveryState(ctx, b.NodeID)
	if err != nil || state.Pages != 4 || state.Seen != 4 || state.Status != "collecting" {
		t.Fatalf("budget refusal changed progress: %+v %v", state, err)
	}
	var pages, uses int
	s.db.QueryRow(`SELECT count(*) FROM recovery_pages`).Scan(&pages)
	s.db.QueryRow(`SELECT uses FROM keys`).Scan(&uses)
	if pages != 4 || uses != 4 {
		t.Fatalf("budget refusal persisted/used key pages=%d uses=%d", pages, uses)
	}
}

func TestRecoveryPagesMemberPhaseIndexMatchesOriginalBody(t *testing.T) {
	for _, phase := range []string{"ready", "START_COMMITTED"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			s, b, _ := recoveryStoreFixture(t)
			q, err := s.StartRecovery(ctx, b, "all", 1)
			if err != nil {
				t.Fatal(err)
			}
			record := recoveryPageRecord("00000000-0000-4000-8000-000000000010")
			record["phase"] = phase
			if phase == "START_COMMITTED" {
				record["last_event_seq"] = 1
			}
			st, err := s.ReceiveRecoveryPage(ctx, b, recoveryPageReply(t, s, b, q, 1, []any{record}, nil))
			if err != nil {
				t.Fatal(err)
			}
			var storedPhase string
			if err = s.db.QueryRow(`SELECT phase FROM recovery_members WHERE round_id=? AND ordinal=0`, st.RoundID).Scan(&storedPhase); err != nil {
				t.Fatalf("phase index missing: %v", err)
			}
			if storedPhase != phase {
				t.Fatalf("phase index %q differs from record %q", storedPhase, phase)
			}
			var indexes int
			if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name='recovery_members_operation_phase'`).Scan(&indexes); err != nil || indexes != 1 {
				t.Fatalf("operation/phase lookup index missing: %d %v", indexes, err)
			}
			if _, err = s.ReadRecoveryPage(ctx, st.RoundID, 0); err != nil {
				t.Fatal(err)
			}
			changed := "START_COMMITTED"
			if phase == changed {
				changed = "ready"
			}
			if _, err = s.db.Exec(`UPDATE recovery_members SET phase=? WHERE round_id=?`, changed, st.RoundID); err != nil {
				t.Fatal(err)
			}
			if raw, err := s.ReadRecoveryPage(ctx, st.RoundID, 0); !errors.Is(err, ErrEvidence) || len(raw) != 0 {
				t.Fatalf("altered phase index accepted: %s %v", raw, err)
			}
		})
	}
}
