package operations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"docker-manager/internal/protocol"
)

const ctlUser = "00000000-0000-4000-8000-000000000050"
const ctlOther = "00000000-0000-4000-8000-000000000099"

func ctlEncode(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ctlFixture(t *testing.T, kind string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../contracts/examples/agent-v1.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	if err := json.Unmarshal(raw, &messages); err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message["type"] == kind {
			return message
		}
	}
	t.Fatal("fixture missing", kind)
	return nil
}

func ctlDigest(t *testing.T, s *Store, m map[string]any) []byte {
	t.Helper()
	b := m["body"].(map[string]any)
	payload, err := base64.StdEncoding.DecodeString(b["payload_b64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := s.InputDigest(payload)
	if err != nil {
		t.Fatal(err)
	}
	b["input_digest"] = digest
	return payload
}

func ctlSetup(t *testing.T) (*Store, Binding, map[string]any, string, Keys) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "manager.sqlite")
	keys := Keys{DigestID: "manager-digest", EncryptionID: "manager-encryption", DigestKey: bytes.Repeat([]byte{0x35}, 32), EncryptionKey: bytes.Repeat([]byte{0x56}, 32)}
	s, err := Create(ctx, path, "00000000-0000-4000-8000-000000000060", keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	m := ctlFixture(t, "prepare")
	b := Binding{NodeID: m["body"].(map[string]any)["node_id"].(string), LedgerID: "00000000-0000-4000-8000-000000000005", SessionID: m["session_id"].(string)}
	if err := s.BindSessionFixture(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUserFixture(ctx, ctlUser, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"node.read", "config.read", "stack.deploy"} {
		if err := s.SetGrantFixture(ctx, ctlUser, b.NodeID, c, true); err != nil {
			t.Fatal(err)
		}
	}
	ctlDigest(t, s, m)
	return s, b, m, path, keys
}

func ctlReady(t *testing.T, s *Store, b Binding, m map[string]any) (Operation, []byte) {
	t.Helper()
	ctx := context.Background()
	op, err := s.AcceptFixture(ctx, b, ctlUser, ctlEncode(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordReadyFixture(ctx, b, op.OperationID, op.Digest); err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewMessage("request_start", b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": ctlOther})
	if err != nil {
		t.Fatal(err)
	}
	return op, req
}

func TestControlPersistentPermissionAndReplay(t *testing.T) {
	s, b, m, _, _ := ctlSetup(t)
	ctx := context.Background()
	op, req := ctlReady(t, s, b, m)
	first, err := s.AuthorizeStart(ctx, b, req)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	json.Unmarshal(first, &before)
	row, err := s.Get(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "authorized" || row.AuthRev != 4 || row.PermitID == "" || row.Revision != 3 {
		t.Fatalf("permit fact: %+v", row)
	}
	if err := s.SetGrantFixture(ctx, ctlUser, b.NodeID, "stack.deploy", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestCancelFixture(ctx, op.OperationID); err != nil {
		t.Fatal(err)
	}
	retry, err := s.AuthorizeStart(ctx, b, req)
	if err != nil {
		t.Fatal("historical permit lost", err)
	}
	json.Unmarshal(retry, &after)
	if !bytes.Equal(ctlEncode(t, before["body"]), ctlEncode(t, after["body"])) {
		t.Fatal("historical permit changed")
	}
	got, _ := s.Get(ctx, op.OperationID)
	if got.PermitID != row.PermitID || got.AuthRev != row.AuthRev || got.Revision != row.Revision+1 || !got.CancelRequested {
		t.Fatalf("replay changed facts: %+v", got)
	}
	intents, err := s.Intents(ctx, op.OperationID)
	if err != nil || len(intents) != 2 {
		t.Fatalf("intents: %+v %v", intents, err)
	}
	for _, intent := range intents {
		if intent.Kind == "authorize_start" {
			t.Fatal("permit entered outbox")
		}
	}
	var request map[string]any
	json.Unmarshal(req, &request)
	request["body"].(map[string]any)["start_nonce"] = "00000000-0000-4000-8000-000000000098"
	if _, err = s.AuthorizeStart(ctx, b, ctlEncode(t, request)); !errors.Is(err, ErrConflict) {
		t.Fatal("new nonce", err)
	}
}

func TestControlPermissionsPreventNewAuthorization(t *testing.T) {
	for _, mode := range []string{"node.read", "config.read", "stack.deploy", "disabled", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, b, m, _, _ := ctlSetup(t)
			ctx := context.Background()
			op, req := ctlReady(t, s, b, m)
			want := ErrPermission
			switch mode {
			case "disabled":
				if err := s.SetUserEnabledFixture(ctx, ctlUser, false); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				if err := s.RequestCancelFixture(ctx, op.OperationID); err != nil {
					t.Fatal(err)
				}
				want = ErrConflict
			default:
				if err := s.SetGrantFixture(ctx, ctlUser, b.NodeID, mode, false); err != nil {
					t.Fatal(err)
				}
			}
			if ack, err := s.AuthorizeStart(ctx, b, req); !errors.Is(err, want) || len(ack) != 0 {
				t.Fatalf("new permit: %s %v", ack, err)
			}
			got, err := s.Get(ctx, op.OperationID)
			if err != nil || got.PermitID != "" || got.State != "ready" {
				t.Fatalf("partial permit: %+v %v", got, err)
			}
		})
	}
}

func TestControlRevokeAndPermitSerialize(t *testing.T) {
	for i := 0; i < 8; i++ {
		s, b, m, _, _ := ctlSetup(t)
		ctx := context.Background()
		op, req := ctlReady(t, s, b, m)
		var wg sync.WaitGroup
		var permitErr, revokeErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, permitErr = s.AuthorizeStart(ctx, b, req) }()
		go func() { defer wg.Done(); revokeErr = s.SetGrantFixture(ctx, ctlUser, b.NodeID, "stack.deploy", false) }()
		wg.Wait()
		if revokeErr != nil {
			t.Fatal(revokeErr)
		}
		row, err := s.Get(ctx, op.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if permitErr == nil {
			if row.AuthRev != 4 || row.PermitID == "" {
				t.Fatalf("permit used post-revocation revision: %+v", row)
			}
		} else if errors.Is(permitErr, ErrPermission) {
			if row.PermitID != "" {
				t.Fatal("denied but persisted permit")
			}
		} else {
			t.Fatal(permitErr)
		}
		var rev uint64
		if err := s.db.QueryRow("SELECT auth_rev FROM users WHERE id=?", ctlUser).Scan(&rev); err != nil || rev != 5 {
			t.Fatalf("grant revision: %d %v", rev, err)
		}
	}
}

func TestControlSessionReplacementAndOpenFreeze(t *testing.T) {
	s, b, m, path, keys := ctlSetup(t)
	ctx := context.Background()
	op, req := ctlReady(t, s, b, m)
	b2 := b
	b2.SessionID = ctlOther
	if err := s.BindSessionFixture(ctx, b2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeStart(ctx, b, req); !errors.Is(err, ErrFrozen) {
		t.Fatal("old session", err)
	}
	if _, err := s.AcceptFixture(ctx, b2, ctlUser, ctlEncode(t, m)); err == nil {
		t.Fatal("replacement accepted prepare")
	}
	reopened, err := Open(ctx, path, s.Identity(), keys)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := s.RequestCancelFixture(ctx, op.OperationID); !errors.Is(err, ErrFrozen) {
		t.Fatal("old store wrote", err)
	}
	if err := reopened.BindSessionFixture(ctx, b2); err != nil {
		t.Fatal(err)
	}
	tx, err := reopened.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := reopened.requireBinding(ctx, tx, b2, true); err != nil {
		t.Fatal("frozen report refused", err)
	}
	if err := reopened.requireBinding(ctx, tx, b2, false); !errors.Is(err, ErrFrozen) {
		t.Fatal("frozen authorization permitted", err)
	}
}

func TestControlGrantChangesAreIdempotent(t *testing.T) {
	s, b, _, _, _ := ctlSetup(t)
	ctx := context.Background()
	if err := s.SetGrantFixture(ctx, ctlUser, b.NodeID, "stack.deploy", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserEnabledFixture(ctx, ctlUser, true); err != nil {
		t.Fatal(err)
	}
	var revision int
	if err := s.db.QueryRow("SELECT auth_rev FROM users WHERE id=?", ctlUser).Scan(&revision); err != nil || revision != 4 {
		t.Fatalf("no-op revision: %d %v", revision, err)
	}
	if err := s.SetGrantFixture(ctx, ctlUser, b.NodeID, "system.admin", true); !errors.Is(err, ErrInput) {
		t.Fatal("invalid capability", err)
	}
}

func TestControlRejectsHiddenResultBeforeChangingFacts(t *testing.T) {
	for _, action := range []string{"ready", "authorize", "cancel", "accept"} {
		t.Run(action, func(t *testing.T) {
			s, _, b, op := eventFixture(t, true)
			ctx := context.Background()
			eventReceive(t, s, b, eventResult(t, s, b, op, "normal_exit", 0, "complete", "consistent", ""))
			if _, err := s.db.Exec(`UPDATE operations SET state='ready',latest_result_seq=0,max_event_seq=0,release_status='held',permit_id='',permit_session='',start_nonce='',auth_rev=0 WHERE operation_id=?`, op.OperationID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`DELETE FROM outbox WHERE kind='finalize' AND operation_id=?`, op.OperationID); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "ready":
				err = s.RecordReadyFixture(ctx, b, op.OperationID, op.Digest)
			case "authorize":
				req, _ := protocol.NewMessage("request_start", b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": ctlOther})
				_, err = s.AuthorizeStart(ctx, b, req)
			case "cancel":
				err = s.RequestCancelFixture(ctx, op.OperationID)
			case "accept":
				_, raw, _ := eventPrepare(t, s)
				_, err = s.AcceptFixture(ctx, b, eventUser, raw)
			}
			if !errors.Is(err, ErrEvidence) {
				t.Fatalf("hidden result accepted for %s: %v", action, err)
			}
			var permit string
			var canceled bool
			if err := s.db.QueryRow("SELECT permit_id,cancel_requested FROM operations WHERE operation_id=?", op.OperationID).Scan(&permit, &canceled); err != nil || permit != "" || canceled {
				t.Fatalf("damaged projection mutated: %q %t %v", permit, canceled, err)
			}
		})
	}
}
