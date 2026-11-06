package ledger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func prepareFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../contracts/examples/agent-v1.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	if err := json.Unmarshal(raw, &messages); err != nil {
		t.Fatal(err)
	}
	for _, msg := range messages {
		if msg["type"] == "prepare" {
			return msg
		}
	}
	t.Fatal("prepare fixture missing")
	return nil
}

func encodeFixture(t *testing.T, message map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func changePayload(t *testing.T, message map[string]any, change func(map[string]any)) {
	t.Helper()
	body := message["body"].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(body["payload_b64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	change(payload)
	body["payload_b64"] = base64.StdEncoding.EncodeToString(encodeFixture(t, payload))
}

func TestRecordReadyPersistsRawFingerprintClaimsAndReplay(t *testing.T) {
	s, _, key := newTestStore(t)
	ctx := context.Background()
	message := prepareFixture(t)
	payload, _ := base64.StdEncoding.DecodeString(message["body"].(map[string]any)["payload_b64"].(string))
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	key[0] ^= 0xff // Store must retain its own key copy.
	record, err := s.RecordReadyFixture(ctx, encodeFixture(t, message))
	if err != nil {
		t.Fatal(err)
	}
	if record.Identity != s.Identity() || record.Phase != "ready" || record.BootID != s.boot || record.Permit != nil || record.StartedAt != "" || record.ProjectName != "sample" || record.PolicyRevision != 1 {
		t.Fatalf("unexpected ready: %+v", record)
	}
	if record.LocalFingerprint != base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("fingerprint is not original payload HMAC or Store did not copy key")
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", record.CreatedAt); err != nil {
		t.Fatal(err)
	}
	message["message_id"] = "00000000-0000-4000-8000-000000000199"
	replayed, err := s.RecordReadyFixture(ctx, encodeFixture(t, message))
	if err != nil || !reflect.DeepEqual(record, replayed) {
		t.Fatalf("replay: %+v %v", replayed, err)
	}
	got, err := s.Get(ctx, record.OperationID)
	if err != nil || !reflect.DeepEqual(record, got) {
		t.Fatalf("read: %+v %v", got, err)
	}
	claims, err := s.Claims(ctx, record.OperationID)
	want := []Claim{{Kind: "project", ResourceID: `["00000000-0000-4000-8000-000000000009","sample"]`}, {Kind: "stack", ResourceID: "00000000-0000-4000-8000-000000000002"}}
	if err != nil || !reflect.DeepEqual(claims, want) {
		t.Fatalf("claims: %+v %v", claims, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM executions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("count %d: %v", count, err)
	}
	var timestampType string
	if err := s.db.QueryRow("SELECT typeof(created_at) FROM executions").Scan(&timestampType); err != nil || timestampType != "integer" {
		t.Fatalf("timestamp storage: %s %v", timestampType, err)
	}
	if _, err := s.Get(ctx, "00000000-0000-4000-8000-000000000099"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRecordReadyRejectsChangedFixedInputs(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	original := prepareFixture(t)
	record, err := s.RecordReadyFixture(ctx, encodeFixture(t, original))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(map[string]any){
		"digest": func(m map[string]any) {
			m["body"].(map[string]any)["input_digest"] = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
		},
		"session": func(m map[string]any) { m["session_id"] = "00000000-0000-4000-8000-000000000099" },
		"bytes": func(m map[string]any) {
			b := m["body"].(map[string]any)
			raw, _ := base64.StdEncoding.DecodeString(b["payload_b64"].(string))
			b["payload_b64"] = base64.StdEncoding.EncodeToString(append(raw, ' '))
		},
		"stack": func(m map[string]any) {
			m["body"].(map[string]any)["stack_id"] = "00000000-0000-4000-8000-000000000099"
			changePayload(t, m, func(p map[string]any) { p["stack_id"] = "00000000-0000-4000-8000-000000000099" })
		},
		"version": func(m map[string]any) {
			m["body"].(map[string]any)["version_id"] = "00000000-0000-4000-8000-000000000099"
			changePayload(t, m, func(p map[string]any) { p["version_id"] = "00000000-0000-4000-8000-000000000099" })
		},
		"project": func(m map[string]any) { changePayload(t, m, func(p map[string]any) { p["project_name"] = "other" }) },
		"endpoint": func(m map[string]any) {
			changePayload(t, m, func(p map[string]any) {
				p["execution_profile"].(map[string]any)["docker_endpoint_id"] = "00000000-0000-4000-8000-000000000099"
			})
		},
		"revision": func(m map[string]any) {
			changePayload(t, m, func(p map[string]any) { p["execution_profile"].(map[string]any)["policy_revision"] = 2 })
		},
		"config": func(m map[string]any) {
			changePayload(t, m, func(p map[string]any) { p["config"].(map[string]any)["profiles"] = []string{"changed"} })
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := prepareFixture(t)
			change(m)
			if _, err := s.RecordReadyFixture(ctx, encodeFixture(t, m)); !errors.Is(err, ErrConflict) {
				t.Fatalf("want conflict, got %v", err)
			}
		})
	}
	got, err := s.Get(ctx, record.OperationID)
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatal("conflict overwrote record", err)
	}
}

func TestRecordReadyAcceptsMathematicalIntegerButPreservesBytes(t *testing.T) {
	for _, token := range []string{"1", "1.0", "1e0", "9007199254740991"} {
		t.Run(token, func(t *testing.T) {
			s, _, _ := newTestStore(t)
			m := prepareFixture(t)
			b := m["body"].(map[string]any)
			payload, _ := base64.StdEncoding.DecodeString(b["payload_b64"].(string))
			payload = bytes.Replace(payload, []byte(`"policy_revision":1`), []byte(`"policy_revision":`+token), 1)
			b["payload_b64"] = base64.StdEncoding.EncodeToString(payload)
			r, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, m))
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(1)
			if token == "9007199254740991" {
				want = 9007199254740991
			}
			if r.PolicyRevision != want {
				t.Fatalf("revision: %d", r.PolicyRevision)
			}
			if token != "1" {
				if _, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t))); !errors.Is(err, ErrConflict) {
					t.Fatalf("raw change should conflict: %v", err)
				}
			}
		})
	}
}

func TestRecordReadyConcurrentClaimsAreAtomic(t *testing.T) {
	for _, resource := range []string{"stack", "project"} {
		t.Run(resource, func(t *testing.T) {
			s, path, key := newTestStore(t)
			second, err := Open(context.Background(), path, s.Identity(), key)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			stores := []*Store{s, second}
			var wg sync.WaitGroup
			results := make(chan error, 8)
			for i := 0; i < 8; i++ {
				m := prepareFixture(t)
				b := m["body"].(map[string]any)
				b["operation_id"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", 1000+i)
				if resource == "project" {
					stack := fmt.Sprintf("00000000-0000-4000-8000-%012d", 2000+i)
					b["stack_id"] = stack
					changePayload(t, m, func(p map[string]any) { p["stack_id"] = stack })
				} else {
					changePayload(t, m, func(p map[string]any) { p["project_name"] = fmt.Sprintf("project%d", i) })
				}
				raw := encodeFixture(t, m)
				wg.Add(1)
				store := stores[i%len(stores)]
				go func() { defer wg.Done(); _, err := store.RecordReadyFixture(context.Background(), raw); results <- err }()
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
				t.Fatalf("successful operations: %d", success)
			}
			var executions, claims int
			s.db.QueryRow("SELECT count(*) FROM executions").Scan(&executions)
			s.db.QueryRow("SELECT count(*) FROM local_claims").Scan(&claims)
			if executions != 1 || claims != 2 {
				t.Fatalf("partial transaction: %d executions %d claims", executions, claims)
			}
		})
	}
}

func TestRecordReadyDifferentEndpointAndFrozenBoot(t *testing.T) {
	s, path, key := newTestStore(t)
	ctx := context.Background()
	m := prepareFixture(t)
	first, err := s.RecordReadyFixture(ctx, encodeFixture(t, m))
	if err != nil {
		t.Fatal(err)
	}
	m["body"].(map[string]any)["operation_id"] = "00000000-0000-4000-8000-000000000099"
	m["body"].(map[string]any)["stack_id"] = "00000000-0000-4000-8000-000000000098"
	changePayload(t, m, func(p map[string]any) {
		p["stack_id"] = "00000000-0000-4000-8000-000000000098"
		p["execution_profile"].(map[string]any)["docker_endpoint_id"] = "00000000-0000-4000-8000-000000000097"
	})
	if _, err := s.RecordReadyFixture(ctx, encodeFixture(t, m)); err != nil {
		t.Fatal(err)
	}
	id := s.Identity()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReadyFixture(ctx, encodeFixture(t, m)); !errors.Is(err, ErrStorage) {
		t.Fatalf("closed store: %v", err)
	}
	reopened, err := Open(ctx, path, id, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Get(ctx, first.OperationID)
	if err != nil || !reflect.DeepEqual(first, got) {
		t.Fatalf("reopened record changed: %+v %v", got, err)
	}
	if _, err := reopened.RecordReadyFixture(ctx, encodeFixture(t, prepareFixture(t))); !errors.Is(err, ErrFrozen) {
		t.Fatalf("old boot: %v", err)
	}
}

func TestRecordReadyInvalidInputAndNodeAreRejected(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	for _, raw := range [][]byte{[]byte(`{"secret":"private"}`), []byte(`not-json`)} {
		if _, err := s.RecordReadyFixture(ctx, raw); !errors.Is(err, ErrInput) {
			t.Fatalf("invalid prepare: %v", err)
		}
	}
	m := prepareFixture(t)
	m["body"].(map[string]any)["node_id"] = "00000000-0000-4000-8000-000000000099"
	changePayload(t, m, func(p map[string]any) { p["node_id"] = "00000000-0000-4000-8000-000000000099" })
	if _, err := s.RecordReadyFixture(ctx, encodeFixture(t, m)); !errors.Is(err, ErrIdentity) {
		t.Fatalf("wrong node: %v", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM executions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid input persisted: %d %v", count, err)
	}
}

func TestRecordReadyStatementFailureRollsBackBothClaims(t *testing.T) {
	s, _, _ := newTestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER abort_second_claim BEFORE INSERT ON local_claims WHEN NEW.resource_kind='project' BEGIN SELECT RAISE(ABORT,'private-config-secret'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t))); err != ErrStorage {
		t.Fatalf("driver details exposed or error missing: %v", err)
	}
	for _, table := range []string{"executions", "local_claims"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial write %s: %d %v", table, count, err)
		}
	}
}

func TestRecordReadyDoesNotPersistConfigurationOrKey(t *testing.T) {
	s, path, key := newTestStore(t)
	m := prepareFixture(t)
	secret := []byte("ledger-configuration-secret-781463118073261219")
	encoded := base64.StdEncoding.EncodeToString(secret)
	changePayload(t, m, func(p map[string]any) {
		p["config"].(map[string]any)["files"].([]any)[0].(map[string]any)["content_b64"] = encoded
	})
	if _, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, m)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.key, make([]byte, 32)) {
		t.Fatal("Close did not clear owned key")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{secret, []byte(encoded), key, []byte(m["body"].(map[string]any)["payload_b64"].(string))} {
		if bytes.Contains(data, value) {
			t.Fatal("ledger contains raw configuration or secret key")
		}
	}
}
