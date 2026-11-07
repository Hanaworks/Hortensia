package operations

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const storageManagerID = "00000000-0000-4000-8000-000000000001"

func storageKeys() Keys {
	return Keys{DigestID: "manager-digest-1", EncryptionID: "manager-encryption-1", DigestKey: bytes.Repeat([]byte{0x31}, 32), EncryptionKey: bytes.Repeat([]byte{0x72}, 32)}
}

func storageFixture(t *testing.T) (*Store, string, Keys) {
	t.Helper()
	path, keys := filepath.Join(t.TempDir(), "manager.sqlite"), storageKeys()
	s, err := Create(context.Background(), path, storageManagerID, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path, keys
}

func TestStoreCreateOpenKeepsIdentityFreezesNodesAndRevokesOldWriter(t *testing.T) {
	s, path, keys := storageFixture(t)
	ctx := context.Background()
	for query, want := range map[string]string{"SELECT sqlite_version()": "3.54.1", "PRAGMA journal_mode": "wal", "PRAGMA synchronous": "2", "PRAGMA foreign_keys": "1", "PRAGMA busy_timeout": "5000"} {
		var got string
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("invariant %s: %q %v", query, got, err)
		}
	}
	node := "00000000-0000-4000-8000-000000000002"
	if _, err := s.db.Exec(`INSERT INTO nodes(id,ledger_id,session_id,boot_id,frozen) VALUES(?,?,?,?,0)`, node, node, node, s.boot); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, path, s.Identity(), keys)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if opened.Identity() != s.Identity() || opened.boot == s.boot {
		t.Fatal("identity or boot lifetime violated")
	}
	if err := s.requireActive(ctx, s.db); !errors.Is(err, ErrFrozen) {
		t.Fatalf("old writer still active: %v", err)
	}
	if err := opened.requireActive(ctx, opened.db); err != nil {
		t.Fatal(err)
	}
	var recovery, frozen int
	if err := opened.db.QueryRow(`SELECT recovery_mode FROM manager_meta WHERE singleton=1`).Scan(&recovery); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRow(`SELECT frozen FROM nodes WHERE id=?`, node).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if recovery != 1 || frozen != 1 {
		t.Fatal("Open did not establish recovery barrier")
	}
	if _, err := Create(ctx, path, storageManagerID, keys); !errors.Is(err, ErrStorage) {
		t.Fatalf("existing database replaced: %v", err)
	}
}

func TestStoreRejectsWrongIdentityKeysVersionAndStructure(t *testing.T) {
	for _, change := range []string{"identity", "digest", "encryption", "digest-id", "encryption-id", "version", "extra-table", "missing-table"} {
		t.Run(change, func(t *testing.T) {
			s, path, keys := storageFixture(t)
			id := s.Identity()
			want := ErrKey
			switch change {
			case "identity":
				id.ManagerID = "00000000-0000-4000-8000-000000000099"
				want = ErrIdentity
			case "digest":
				keys.DigestKey[0] ^= 1
			case "encryption":
				keys.EncryptionKey[0] ^= 1
			case "digest-id":
				keys.DigestID += "-bad"
			case "encryption-id":
				keys.EncryptionID += "-bad"
			case "version":
				_, _ = s.db.Exec(`PRAGMA user_version=2`)
				want = ErrSchema
			case "extra-table":
				_, _ = s.db.Exec(`CREATE TABLE unauthorized(x TEXT)`)
				want = ErrSchema
			case "missing-table":
				_, _ = s.db.Exec(`DROP TABLE event_conflicts`)
				want = ErrSchema
			}
			got, err := Open(context.Background(), path, id, keys)
			if got != nil {
				got.Close()
				t.Fatal("unexpected store returned")
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
		})
	}
}

func TestStoreRejectsUnsafePathsAndInvalidKeysWithoutCreatingFiles(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"", ":memory:", "file:test", "a?mode=memory", "a\x00b", `\\server\share\db`, "//server/share/db"} {
		if s, err := Create(ctx, path, storageManagerID, storageKeys()); !errors.Is(err, ErrInput) || s != nil {
			t.Fatalf("accepted path %q: %v", path, err)
		}
	}
	for _, which := range []string{"short", "same-key", "same-id", "empty-id"} {
		keys := storageKeys()
		switch which {
		case "short":
			keys.DigestKey = keys.DigestKey[:31]
		case "same-key":
			keys.EncryptionKey = bytes.Clone(keys.DigestKey)
		case "same-id":
			keys.EncryptionID = keys.DigestID
		case "empty-id":
			keys.EncryptionID = ""
		}
		path := filepath.Join(t.TempDir(), "absent.sqlite")
		if s, err := Create(ctx, path, storageManagerID, keys); !errors.Is(err, ErrKey) || s != nil {
			t.Fatalf("accepted keys %s: %v", which, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid keys created database")
		}
	}
}

func TestStoreCloseClearsOnlyOwnedKeysAndRejectsLaterWork(t *testing.T) {
	s, _, keys := storageFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if !bytes.Equal(s.keys.DigestKey, make([]byte, 32)) || !bytes.Equal(s.keys.EncryptionKey, make([]byte, 32)) {
		t.Fatal("owned keys retained")
	}
	if bytes.Equal(keys.DigestKey, make([]byte, 32)) || bytes.Equal(keys.EncryptionKey, make([]byte, 32)) {
		t.Fatal("caller keys modified")
	}
	if _, err := s.InputDigest([]byte("secret")); !errors.Is(err, ErrStorage) {
		t.Fatalf("closed digest accepted: %v", err)
	}
	if err := s.requireActive(context.Background(), s.db); !errors.Is(err, ErrStorage) {
		t.Fatalf("closed store accepted: %v", err)
	}
}
