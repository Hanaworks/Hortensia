package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"docker-manager/internal/protocol"
)

const mfUserID = "00000000-0000-4000-8000-000000000021"
const mfManagerID = "00000000-0000-4000-8000-000000000022"

func mfKeys() Keys {
	return Keys{DigestID: "fault-digest", EncryptionID: "fault-encryption", DigestKey: bytes.Repeat([]byte{0x51}, 32), EncryptionKey: bytes.Repeat([]byte{0x62}, 32)}
}

func mfExample(t *testing.T, kind string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "examples", "agent-v1.valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	var examples []map[string]any
	if err := json.Unmarshal(raw, &examples); err != nil {
		t.Fatal(err)
	}
	for _, example := range examples {
		if example["type"] == kind {
			return example
		}
	}
	t.Fatalf("missing contract example %s", kind)
	return nil
}

func mfEncode(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The helper creates its own database: opening a parent-created database would
// correctly freeze authorization before the process-boundary exercise began.
func mfFixtureAt(t *testing.T, path string) (*Store, Binding, []byte) {
	t.Helper()
	ctx := context.Background()
	s, err := Create(ctx, path, mfManagerID, mfKeys())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	prepare := mfExample(t, "prepare")
	body := prepare["body"].(map[string]any)
	b := Binding{NodeID: body["node_id"].(string), LedgerID: "00000000-0000-4000-8000-000000000005", SessionID: prepare["session_id"].(string)}
	if err := s.BindSessionFixture(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUserFixture(ctx, mfUserID, true); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"node.read", "config.read", "stack.deploy"} {
		if err := s.SetGrantFixture(ctx, mfUserID, b.NodeID, capability, true); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := base64.StdEncoding.DecodeString(body["payload_b64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := s.InputDigest(payload)
	if err != nil {
		t.Fatal(err)
	}
	body["input_digest"] = digest
	return s, b, mfEncode(t, prepare)
}

func mfRequest(t *testing.T, b Binding, op Operation) []byte {
	t.Helper()
	request := mfExample(t, "request_start")
	request["session_id"] = b.SessionID
	body := request["body"].(map[string]any)
	body["operation_id"], body["input_digest"] = op.OperationID, op.Digest
	return mfEncode(t, request)
}

func mfAuthorized(t *testing.T, s *Store, b Binding, prepare []byte) Operation {
	t.Helper()
	ctx := context.Background()
	op, err := s.AcceptFixture(ctx, b, mfUserID, prepare)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReadyFixture(ctx, b, op.OperationID, op.Digest); err != nil {
		t.Fatal(err)
	}
	if permit, err := s.AuthorizeStart(ctx, b, mfRequest(t, b, op)); err != nil || len(permit) == 0 {
		t.Fatalf("authorize: %v", err)
	}
	op, err = s.Get(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func mfEvent(t *testing.T, b Binding, op Operation, kind string) []byte {
	t.Helper()
	message := mfExample(t, kind)
	message["session_id"] = b.SessionID
	body := message["body"].(map[string]any)
	body["operation_id"], body["input_digest"] = op.OperationID, op.Digest
	if kind == "started" {
		body["permit_id"] = op.PermitID
	}
	return mfEncode(t, message)
}

func mfEnvelope(t *testing.T, raw []byte) protocol.Envelope {
	t.Helper()
	var envelope protocol.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func mfCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func mfUses(t *testing.T, s *Store) int {
	t.Helper()
	var uses int
	if err := s.db.QueryRow("SELECT uses FROM keys WHERE singleton=1").Scan(&uses); err != nil {
		t.Fatal(err)
	}
	return uses
}

// Both faults execute inside the actual business transaction. In the second
// case the trigger's INSERT is legal until COMMIT checks the deferred FK.
// TEMP objects leave the persistent schema unchanged for the subsequent Open.
func mfInstallFault(t *testing.T, s *Store, kind string, commitFailure bool) func() {
	t.Helper()
	statement := `CREATE TEMP TRIGGER mf_fail BEFORE INSERT ON main.outbox WHEN NEW.kind='` + kind + `' BEGIN SELECT RAISE(ABORT,'private-manager-fault-secret'); END`
	if commitFailure {
		statement = `CREATE TEMP TABLE mf_parent(id TEXT PRIMARY KEY);
CREATE TEMP TABLE mf_failure(parent TEXT REFERENCES mf_parent(id) DEFERRABLE INITIALLY DEFERRED);
CREATE TEMP TRIGGER mf_fail AFTER INSERT ON main.outbox WHEN NEW.kind='` + kind + `' BEGIN INSERT INTO mf_failure(parent) VALUES('private-manager-fault-secret'); END`
	}
	if _, err := s.db.Exec(statement); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := s.db.Exec("DROP TRIGGER mf_fail"); err != nil {
			t.Fatal(err)
		}
		if commitFailure {
			if mfCount(t, s, "temp.mf_failure") != 0 {
				t.Fatal("deferred failure row survived transaction rollback")
			}
			if _, err := s.db.Exec("DROP TABLE mf_failure; DROP TABLE mf_parent"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func mfStorageFailure(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("expected storage failure, got %v", err)
	}
	if strings.Contains(err.Error(), "private-manager-fault-secret") {
		t.Fatal("SQL fault leaked private data")
	}
}

func TestManagerAcceptanceStatementAndCommitFailuresAreAtomic(t *testing.T) {
	for _, mode := range []string{"statement", "commit"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manager.db")
			s, b, raw := mfFixtureAt(t, path)
			removeFault := mfInstallFault(t, s, "prepare", mode == "commit")
			beforeUses := mfUses(t, s)
			op, err := s.AcceptFixture(context.Background(), b, mfUserID, raw)
			mfStorageFailure(t, err)
			if op.OperationID != "" {
				t.Fatal("failed acceptance returned a successful operation")
			}
			for _, table := range []string{"operations", "resource_claims", "outbox", "operation_events", "event_conflicts"} {
				if count := mfCount(t, s, table); count != 0 {
					t.Fatalf("partial acceptance in %s: %d rows", table, count)
				}
			}
			if got := mfUses(t, s); got != beforeUses+1 {
				t.Fatalf("rolled-back acceptance refunded KEK reservation: %d", got)
			}
			removeFault()
			op, err = s.AcceptFixture(context.Background(), b, mfUserID, raw)
			if err != nil {
				t.Fatal(err)
			}
			if mfUses(t, s) != beforeUses+2 {
				t.Fatal("retry reused a consumed KEK reservation")
			}
			if mfCount(t, s, "operations") != 1 || mfCount(t, s, "resource_claims") != 2 || mfCount(t, s, "outbox") != 1 {
				t.Fatal("retry did not commit the complete acceptance")
			}
			identity := s.Identity()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(context.Background(), path, identity, mfKeys())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			stored, err := reopened.Get(context.Background(), op.OperationID)
			if err != nil || stored.State != "accepted" {
				t.Fatalf("acceptance did not survive reopen: %+v %v", stored, err)
			}
			if mfCount(t, reopened, "resource_claims") != 2 || mfUses(t, reopened) != beforeUses+2 {
				t.Fatal("claims or consumed quota changed on reopen")
			}
		})
	}
}

func TestManagerResultStatementAndCommitFailuresAreAtomic(t *testing.T) {
	for _, mode := range []string{"statement", "commit"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "manager.db")
			s, b, prepare := mfFixtureAt(t, path)
			op := mfAuthorized(t, s, b, prepare)
			startedRaw := mfEvent(t, b, op, "started")
			if _, err := s.ReceiveEvent(ctx, b, startedRaw); err != nil {
				t.Fatal(err)
			}
			before, err := s.Get(ctx, op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			beforeIntents, err := s.Intents(ctx, op.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			beforeUses := mfUses(t, s)
			removeFault := mfInstallFault(t, s, "finalize", mode == "commit")
			resultRaw := mfEvent(t, b, op, "result")
			ack, err := s.ReceiveEvent(ctx, b, resultRaw)
			mfStorageFailure(t, err)
			if len(ack) != 0 {
				t.Fatal("result escaped failed commit with an acknowledgement")
			}
			after, err := s.Get(ctx, op.OperationID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("partial result projection: before=%+v after=%+v err=%v", before, after, err)
			}
			intents, err := s.Intents(ctx, op.OperationID)
			if err != nil || !reflect.DeepEqual(beforeIntents, intents) {
				t.Fatalf("failed result changed intents: %+v %v", intents, err)
			}
			if mfCount(t, s, "operation_events") != 1 || mfCount(t, s, "resource_claims") != 2 || mfCount(t, s, "event_conflicts") != 0 {
				t.Fatal("partial result rows or released claims")
			}
			if _, err := s.ReadEvent(ctx, op.OperationID, 2); !errors.Is(err, ErrNotFound) {
				t.Fatalf("uncommitted event readable: %v", err)
			}
			started, err := s.ReadEvent(ctx, op.OperationID, 1)
			if err != nil || !bytes.Equal(started.Body, mfEnvelope(t, startedRaw).Body) {
				t.Fatalf("prior started evidence changed: %v", err)
			}
			if mfUses(t, s) != beforeUses+1 {
				t.Fatal("failed result refunded KEK reservation")
			}
			removeFault()
			ack, err = s.ReceiveEvent(ctx, b, resultRaw)
			if err != nil || len(ack) == 0 {
				t.Fatalf("retry: %v", err)
			}
			if mfUses(t, s) != beforeUses+2 {
				t.Fatal("retry reused consumed reservation")
			}
			after, err = s.Get(ctx, op.OperationID)
			if err != nil || after.State != "succeeded" || after.ReleaseStatus != "pending" || after.LatestResultSeq != 2 || after.MaxEventSeq != 2 {
				t.Fatalf("result retry not atomically projected: %+v %v", after, err)
			}
			mfAssertFinalize(t, s, op.OperationID, true)
			identity := s.Identity()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, path, identity, mfKeys())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			result, err := reopened.ReadEvent(ctx, op.OperationID, 2)
			if err != nil || !bytes.Equal(result.Body, mfEnvelope(t, resultRaw).Body) {
				t.Fatalf("result body not durable: %v", err)
			}
			mfAssertFinalize(t, reopened, op.OperationID, true)
			if mfCount(t, reopened, "resource_claims") != 2 || mfUses(t, reopened) != beforeUses+2 {
				t.Fatal("claims or quota changed on reopen")
			}
		})
	}
}

func mfAssertFinalize(t *testing.T, s *Store, operation string, present bool) {
	t.Helper()
	intents, err := s.Intents(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, intent := range intents {
		if intent.Kind == "finalize" {
			count++
			if intent.State != "pending" || intent.ResultEventSeq != 2 || intent.TerminalState != "succeeded" || intent.Generation != 1 {
				t.Fatalf("wrong finalization intent: %+v", intent)
			}
		}
	}
	want := 0
	if present {
		want = 1
	}
	if count != want {
		t.Fatalf("finalize intents=%d want=%d", count, want)
	}
}
