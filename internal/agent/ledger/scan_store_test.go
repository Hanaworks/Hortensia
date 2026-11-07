package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func recoveryStoreFixture(t *testing.T) (*Store, string, []byte, JournalKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery.sqlite")
	key := bytes.Repeat([]byte{0x42}, 32)
	jk := JournalKey{ID: "recovery-key", Key: bytes.Repeat([]byte{0x73}, 32)}
	s, err := CreateRecovery(context.Background(), path, testNode, "key-1", key, jk, RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, path, key, jk
}

func TestRecoveryStoreVersionIdentityAndInitialFreeze(t *testing.T) {
	s, path, key, jk := recoveryStoreFixture(t)
	ctx := context.Background()
	state, err := s.RecoveryScanState(ctx)
	if err != nil || !state.Frozen || state.SessionID != "" || state.ActiveScanID != "" || state.CompletedScanID != "" || state.Total != 0 {
		t.Fatalf("initial state: %+v %v", state, err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatalf("version %d %v", version, err)
	}
	if _, err := Open(ctx, path, s.Identity(), key); !errors.Is(err, ErrSchema) {
		t.Fatalf("v1 accepted v3: %v", err)
	}
	if _, err := OpenJournal(ctx, path, s.Identity(), key, jk); !errors.Is(err, ErrSchema) {
		t.Fatalf("v2 accepted v3: %v", err)
	}
	wrongIdentity := s.Identity()
	wrongIdentity.LedgerID = gateOtherID
	if _, err := OpenRecovery(ctx, path, wrongIdentity, key, jk, RecoveryOptions{}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("wrong ledger identity: %v", err)
	}
	if _, err := OpenRecovery(ctx, path, s.Identity(), bytes.Repeat([]byte{0x24}, 32), jk, RecoveryOptions{}); !errors.Is(err, ErrKey) {
		t.Fatalf("wrong ledger key: %v", err)
	}
	for _, wrong := range []JournalKey{{ID: "wrong", Key: jk.Key}, {ID: jk.ID, Key: bytes.Repeat([]byte{0x23}, 32)}} {
		if _, err := OpenRecovery(ctx, path, s.Identity(), key, wrong, RecoveryOptions{}); !errors.Is(err, ErrKey) {
			t.Fatalf("wrong KEK: %v", err)
		}
	}
	v1, p1, k1 := newTestStore(t)
	if _, err := OpenRecovery(ctx, p1, v1.Identity(), k1, jk, RecoveryOptions{}); !errors.Is(err, ErrSchema) {
		t.Fatalf("v3 accepted v1: %v", err)
	}
	v2, p2, k2, j2 := newCryptoJournal(t)
	if _, err := OpenRecovery(ctx, p2, v2.Identity(), k2, j2, RecoveryOptions{}); !errors.Is(err, ErrSchema) {
		t.Fatalf("v3 accepted v2: %v", err)
	}
	if _, err := v1.RecoveryScanState(ctx); !errors.Is(err, ErrRecovery) {
		t.Fatalf("v1 recovery API: %v", err)
	}
	if err := v2.BindRecoverySessionFixture(ctx, testNode); !errors.Is(err, ErrRecovery) {
		t.Fatalf("v2 recovery API: %v", err)
	}
}

func TestRecoveryStoreReopenRevokesPriorWriter(t *testing.T) {
	s, path, key, jk := recoveryStoreFixture(t)
	ctx := context.Background()
	if err := s.BindRecoverySessionFixture(ctx, testNode); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenRecovery(ctx, path, s.Identity(), key, jk, RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	state, err := opened.RecoveryScanState(ctx)
	if err != nil || !state.Frozen || state.SessionID != testNode {
		t.Fatalf("reopen: %+v %v", state, err)
	}
	if err := s.BindRecoverySessionFixture(ctx, gateOtherID); !errors.Is(err, ErrFrozen) {
		t.Fatalf("stale binder: %v", err)
	}
	if err := s.reserveKeyUse(ctx, jk.ID); !errors.Is(err, ErrFrozen) {
		t.Fatalf("stale crypto writer: %v", err)
	}
	var uses int
	if err := opened.db.QueryRow("SELECT uses FROM event_keys").Scan(&uses); err != nil || uses != 0 {
		t.Fatalf("stale writer used quota: %d %v", uses, err)
	}
}

func TestRecoveryStoreInvalidCreationAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.sqlite")
	key := bytes.Repeat([]byte{0x42}, 32)
	if _, err := CreateRecovery(context.Background(), path, testNode, "key-1", key, JournalKey{}, RecoveryOptions{}); !errors.Is(err, ErrKey) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid key created file")
	}
	s, p, k, jk := recoveryStoreFixture(t)
	for _, stmt := range []string{
		"UPDATE recovery_control SET frozen=2", "UPDATE recovery_control SET dirty=-1",
		"UPDATE recovery_control SET active_scan_id='scan'", "UPDATE recovery_control SET active_total=-1",
		"INSERT INTO recovery_members VALUES('scan',0,'missing',0,0,'ready',0,zeroblob(32))",
		"INSERT INTO recovery_changes VALUES('missing',1)",
	} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Fatalf("accepted %s", stmt)
		}
	}
	if _, err := s.db.Exec("CREATE TABLE unexpected(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	identity := s.Identity()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRecovery(context.Background(), p, identity, k, jk, RecoveryOptions{}); !errors.Is(err, ErrSchema) {
		t.Fatalf("altered schema: %v", err)
	}
}
