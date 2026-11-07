package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func ctlUses(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT uses FROM keys WHERE singleton=1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecordsAcceptAtomicRawPayloadAndReplay(t *testing.T) {
	s, b, m, _, _ := ctlSetup(t)
	ctx := context.Background()
	payload := ctlDigest(t, s, m)
	op, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "accepted" || op.Revision != 1 || op.ReleaseStatus != "held" || op.OwnerBoot != s.boot {
		t.Fatalf("initial operation: %+v", op)
	}
	claims, err := s.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims: %+v %v", claims, err)
	}
	intents, err := s.Intents(ctx, op.OperationID)
	if err != nil || !reflect.DeepEqual(intents, []Intent{{Kind: "prepare", State: "pending", Generation: 1}}) {
		t.Fatalf("intent: %+v %v", intents, err)
	}
	raw, err := s.ReadPayload(ctx, op.OperationID)
	if err != nil || !bytes.Equal(raw, payload) {
		t.Fatal("payload changed", err)
	}
	uses := ctlUses(t, s)
	m["message_id"] = ctlOther
	replay, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m))
	if err != nil || !reflect.DeepEqual(replay, op) || ctlUses(t, s) != uses {
		t.Fatalf("replay changed fact/uses: %+v %v", replay, err)
	}
	if err := s.RecordReadyFixture(ctx, b, op.OperationID, op.Digest); err != nil {
		t.Fatal(err)
	}
	ready, _ := s.Get(ctx, op.OperationID)
	if err := s.RecordReadyFixture(ctx, b, op.OperationID, op.Digest); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Get(ctx, op.OperationID)
	if ready.Revision != 2 || ready.State != "ready" || !reflect.DeepEqual(ready, again) {
		t.Fatal("ready not idempotent")
	}
	intents, _ = s.Intents(ctx, op.OperationID)
	if intents[0].State != "confirmed" {
		t.Fatal("prepare unconfirmed")
	}
}

func TestRecordsRejectInputDigestAndBytesChanges(t *testing.T) {
	s, b, m, _, _ := ctlSetup(t)
	ctx := context.Background()
	op, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	body := m["body"].(map[string]any)
	original := body["payload_b64"].(string)
	payload, _ := base64.StdEncoding.DecodeString(original)
	body["payload_b64"] = base64.StdEncoding.EncodeToString(append(payload, ' '))
	if _, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m)); !errors.Is(err, ErrConflict) {
		t.Fatal("bad provided digest", err)
	}
	ctlDigest(t, s, m)
	if _, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m)); !errors.Is(err, ErrConflict) {
		t.Fatal("raw bytes not compared", err)
	}
	got, _ := s.Get(ctx, op.OperationID)
	if got.Revision != 1 || ctlUses(t, s) != 1 {
		t.Fatal("conflict mutated record")
	}
}

func TestRecordsPermissionCheckedAtAcceptance(t *testing.T) {
	for _, capability := range []string{"node.read", "config.read", "stack.deploy"} {
		t.Run(capability, func(t *testing.T) {
			s, b, m, _, _ := ctlSetup(t)
			ctx := context.Background()
			if err := s.SetGrantFixture(ctx, ctlUser, b.NodeID, capability, false); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m)); !errors.Is(err, ErrPermission) {
				t.Fatal(err)
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM operations").Scan(&count); err != nil || count != 0 {
				t.Fatal("unauthorized persisted", count, err)
			}
		})
	}
}

func TestRecordsConflictingClaimsAreAtomic(t *testing.T) {
	for _, kind := range []string{"project", "stack"} {
		t.Run(kind, func(t *testing.T) {
			s, b, _, _, _ := ctlSetup(t)
			ctx := context.Background()
			var wg sync.WaitGroup
			results := make(chan error, 5)
			for i := 0; i < 5; i++ {
				m := ctlFixture(t, "prepare")
				body := m["body"].(map[string]any)
				body["operation_id"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", 1000+i)
				payload, _ := base64.StdEncoding.DecodeString(body["payload_b64"].(string))
				var p map[string]any
				json.Unmarshal(payload, &p)
				if kind == "project" {
					id := fmt.Sprintf("00000000-0000-4000-8000-%012d", 2000+i)
					body["stack_id"] = id
					p["stack_id"] = id
				} else {
					p["project_name"] = fmt.Sprintf("project%d", i)
				}
				body["payload_b64"] = base64.StdEncoding.EncodeToString(ctlEncode(t, p))
				ctlDigest(t, s, m)
				raw := ctlEncode(t, m)
				wg.Add(1)
				go func() { defer wg.Done(); _, err := s.AcceptFixture(ctx, b, ctlUser, raw); results <- err }()
			}
			wg.Wait()
			close(results)
			success := 0
			for err := range results {
				if err == nil {
					success++
				} else if !errors.Is(err, ErrClaimed) {
					t.Fatal(err)
				}
			}
			if success != 1 {
				t.Fatal("winners", success)
			}
			for table, want := range map[string]int{"operations": 1, "resource_claims": 2, "outbox": 1} {
				var count int
				if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
					t.Fatalf("partial %s=%d %v", table, count, err)
				}
			}
		})
	}
}

func TestRecordsTransactionFailureHasNoPartialAcceptance(t *testing.T) {
	s, b, m, _, _ := ctlSetup(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_intent BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m)); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	for _, table := range []string{"operations", "resource_claims", "outbox"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("partial commit", table, count, err)
		}
	}
	if ctlUses(t, s) != 1 {
		t.Fatal("failed operation refunded wrapping quota")
	}
}

func TestRecordsReadRejectsPayloadAndClaimCorruption(t *testing.T) {
	for _, field := range []string{"payload_cipher", "wrapped_dek", "fingerprint", "claims"} {
		t.Run(field, func(t *testing.T) {
			s, b, m, _, _ := ctlSetup(t)
			ctx := context.Background()
			op, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m))
			if err != nil {
				t.Fatal(err)
			}
			if field == "claims" {
				_, err = s.db.Exec("DELETE FROM resource_claims WHERE operation_id=? AND resource_kind='stack'", op.OperationID)
			} else {
				_, err = s.db.Exec("UPDATE operations SET "+field+"=zeroblob(length("+field+")) WHERE operation_id=?", op.OperationID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Get(ctx, op.OperationID); !errors.Is(err, ErrEvidence) {
				t.Fatal("Get hid corrupt evidence", err)
			}
			if field != "claims" {
				if _, err = s.ReadPayload(ctx, op.OperationID); !errors.Is(err, ErrEvidence) {
					t.Fatal("ReadPayload hid corruption", err)
				}
			}
		})
	}
}

func TestRecordsPreserveMathematicalIntegerPayload(t *testing.T) {
	for _, token := range []string{"1.0", "1e0", "9007199254740991"} {
		t.Run(token, func(t *testing.T) {
			s, b, m, _, _ := ctlSetup(t)
			body := m["body"].(map[string]any)
			raw, _ := base64.StdEncoding.DecodeString(body["payload_b64"].(string))
			raw = bytes.Replace(raw, []byte(`"policy_revision":1`), []byte(`"policy_revision":`+token), 1)
			raw = append(raw, '\n', ' ')
			body["payload_b64"] = base64.StdEncoding.EncodeToString(raw)
			ctlDigest(t, s, m)
			op, err := s.AcceptFixture(context.Background(), b, ctlUser, ctlEncode(t, m))
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(1)
			if token == "9007199254740991" {
				want = 9007199254740991
			}
			if op.PolicyRevision != want {
				t.Fatalf("revision %d want %d", op.PolicyRevision, want)
			}
			got, err := s.ReadPayload(context.Background(), op.OperationID)
			if err != nil || !bytes.Equal(raw, got) {
				t.Fatal("payload normalized", err)
			}
		})
	}
}

func TestRecordsRejectChangedPayloadMetadata(t *testing.T) {
	for _, change := range []string{"project_name='another'", "endpoint_id='00000000-0000-4000-8000-000000000099'", "policy_revision=2", "stack_id='00000000-0000-4000-8000-000000000099'"} {
		t.Run(change, func(t *testing.T) {
			s, b, m, _, _ := ctlSetup(t)
			op, err := s.AcceptFixture(context.Background(), b, ctlUser, ctlEncode(t, m))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("UPDATE operations SET "+change+" WHERE operation_id=?", op.OperationID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.ReadPayload(context.Background(), op.OperationID); !errors.Is(err, ErrEvidence) {
				t.Fatal("metadata no longer binds payload", err)
			}
		})
	}
}

func TestRecordsRejectMissingControlIntent(t *testing.T) {
	for _, kind := range []string{"prepare", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, b, m, _, _ := ctlSetup(t)
			ctx := context.Background()
			op, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RequestCancelFixture(ctx, op.OperationID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("DELETE FROM outbox WHERE operation_id=? AND kind=?", op.OperationID, kind); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Get(ctx, op.OperationID); !errors.Is(err, ErrEvidence) {
				t.Errorf("Get hid missing %s intent: %v", kind, err)
			}
			if _, err = s.Intents(ctx, op.OperationID); !errors.Is(err, ErrEvidence) {
				t.Errorf("Intents hid missing %s intent: %v", kind, err)
			}
		})
	}
}
