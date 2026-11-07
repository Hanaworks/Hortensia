package operations

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
)

const recoveryAgentBoot = "00000000-0000-4000-8000-000000000091"

func recoveryStoreFixture(t *testing.T) (*Store, Binding, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery.sqlite")
	s, err := CreateRecovery(context.Background(), path, storageManagerID, storageKeys())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := Binding{NodeID: "00000000-0000-4000-8000-000000000002", LedgerID: "00000000-0000-4000-8000-000000000003", SessionID: "00000000-0000-4000-8000-000000000004"}
	if err = s.BindRecoverySessionFixture(context.Background(), b, recoveryAgentBoot); err != nil {
		t.Fatal(err)
	}
	return s, b, path
}
func TestRecoveryStoreVersionsFreezeAndBoot(t *testing.T) {
	ctx := context.Background()
	s, b, path := recoveryStoreFixture(t)
	if err := s.requireBinding(ctx, s.db, b, false); !errors.Is(err, ErrFrozen) {
		t.Fatalf("new node admitted: %v", err)
	}
	if err := s.BindSessionFixture(ctx, b); !errors.Is(err, ErrSchema) {
		t.Fatalf("legacy bind accepted: %v", err)
	}
	if got, err := Open(ctx, path, s.Identity(), storageKeys()); err != ErrSchema || got != nil {
		t.Fatalf("v1 open v2: %v", err)
	}
	request, err := s.StartRecovery(ctx, b, "all", 2)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.StartRecovery(ctx, b, "all", 2)
	if err != nil || !bytes.Equal(request, replay) {
		t.Fatalf("start replay %v", err)
	}
	state, err := s.ReadRecoveryState(ctx, b.NodeID)
	if err != nil || state.Total != -1 || state.Status != "collecting" {
		t.Fatalf("state %+v %v", state, err)
	}
	if err = s.BindRecoverySessionFixture(ctx, b, "00000000-0000-4000-8000-000000000092"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadRecoveryState(ctx, b.NodeID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old round still current: %v", err)
	}
	var status string
	if err = s.db.QueryRow(`SELECT status FROM recovery_scans WHERE id=?`, state.RoundID).Scan(&status); err != nil || status != "abandoned" {
		t.Fatalf("old round %s %v", status, err)
	}
	opened, err := OpenRecovery(ctx, path, s.Identity(), storageKeys())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err = s.StartRecovery(ctx, b, "all", 2); !errors.Is(err, ErrFrozen) {
		t.Fatalf("old store writes: %v", err)
	}
	if err = opened.BindRecoverySessionFixture(ctx, b, recoveryAgentBoot); err != nil {
		t.Fatal(err)
	}
	if err = opened.requireBinding(ctx, opened.db, b, false); !errors.Is(err, ErrFrozen) {
		t.Fatalf("restart admitted: %v", err)
	}
	v1, p, k := storageFixture(t)
	if got, err := OpenRecovery(ctx, p, v1.Identity(), k); err != ErrSchema || got != nil {
		t.Fatalf("v2 open v1: %v", err)
	}
}

func TestRecoveryStoreDiagnosticReadPreservesStorageFailure(t *testing.T) {
	s, b, _ := recoveryStoreFixture(t)
	if _, err := s.db.Exec(`DROP TABLE recovery_nodes`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadRecoveryState(context.Background(), b.NodeID); !errors.Is(err, ErrStorage) {
		t.Fatalf("broken storage mistaken for absent round: %v", err)
	}
}
