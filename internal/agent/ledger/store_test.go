package ledger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
)

const testNode = "00000000-0000-4000-8000-000000000001"

func newTestStore(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.sqlite")
	key := bytes.Repeat([]byte{0x42}, 32)
	s, err := Create(context.Background(), path, testNode, "key-1", key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, path, key
}

func TestStoreRejectsUnsafePhysicalConnection(t *testing.T) {
	_, path, _ := newTestStore(t)
	for _, pragma := range []string{"foreign_keys(OFF)", "synchronous(NORMAL)", "busy_timeout(1)"} {
		t.Run(pragma, func(t *testing.T) {
			driver := &sqlite.Driver{}
			conn, err := driver.Open(databaseURI(path))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.(sqlite.ExecQuerierContext).ExecContext(context.Background(), "PRAGMA "+pragma, nil); err != nil {
				t.Fatal(err)
			}
			if err := verifyConnection(conn.(sqlite.ExecQuerierContext), ""); !errors.Is(err, ErrStorage) {
				t.Fatalf("unsafe connection accepted: %v", err)
			}
		})
	}
}

func assertConnectionSettings(t *testing.T, db *sql.DB) {
	t.Helper()
	for query, want := range map[string]string{
		"SELECT sqlite_version()": "3.54.1", "PRAGMA journal_mode": "wal",
		"PRAGMA synchronous": "2", "PRAGMA foreign_keys": "1", "PRAGMA busy_timeout": "5000",
	} {
		var got string
		if err := db.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: got %s, want %s", query, got, want)
		}
	}
}

func TestStoreCreateOpenIdentityAndConnectionSettings(t *testing.T) {
	s, path, key := newTestStore(t)
	identity, boot := s.Identity(), s.boot
	if identity.NodeID != testNode || identity.KeyID != "key-1" || identity.LedgerID == "" || boot == "" {
		t.Fatalf("bad identity: %+v", identity)
	}
	assertConnectionSettings(t, s.db)
	s.db.SetMaxIdleConns(0)
	assertConnectionSettings(t, s.db) // Every query now uses a newly opened physical connection.
	s.db.SetMaxIdleConns(1)
	var check []byte
	if err := s.db.QueryRow("SELECT key_check FROM agent_meta WHERE id = 1").Scan(&check); err != nil {
		t.Fatal(err)
	}
	identityJSON, _ := json.Marshal([]string{identity.NodeID, identity.LedgerID, identity.KeyID})
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("docker-manager/ledger-key/v1\x00"))
	mac.Write(identityJSON)
	if !hmac.Equal(check, mac.Sum(nil)) {
		t.Fatal("key check does not bind all identity fields with the specified domain")
	}
	if _, err := Create(context.Background(), path, testNode, "key-1", key); !errors.Is(err, ErrStorage) {
		t.Fatalf("create existing: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), path, identity, key)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Identity() != identity || reopened.boot == boot {
		t.Fatal("identity changed or boot reused")
	}
	assertConnectionSettings(t, reopened.db)
}

func TestStoreOpenRejectsMissingCorruptionAndIdentityMismatch(t *testing.T) {
	s, path, key := newTestStore(t)
	id := s.Identity()
	for _, tc := range []struct {
		name string
		id   Identity
		key  []byte
		want error
	}{
		{"node", Identity{"00000000-0000-4000-8000-000000000099", id.LedgerID, id.KeyID}, key, ErrIdentity},
		{"ledger", Identity{id.NodeID, "00000000-0000-4000-8000-000000000099", id.KeyID}, key, ErrIdentity},
		{"key ID", Identity{id.NodeID, id.LedgerID, "key-2"}, key, ErrIdentity},
		{"wrong key", id, bytes.Repeat([]byte{3}, 32), ErrKey},
		{"short key", id, key[:31], ErrKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Open(context.Background(), path, tc.id, tc.key); !errors.Is(err, tc.want) {
				if got != nil {
					got.Close()
				}
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := Open(context.Background(), missing, id, key); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Open created a missing ledger")
	}
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	secret := []byte("not a database: secret configuration")
	if err := os.WriteFile(corrupt, secret, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), corrupt, id, key); !errors.Is(err, ErrStorage) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("corrupt: %v", err)
	}
	got, err := os.ReadFile(corrupt)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatal("corrupt ledger changed")
	}
}

func TestStoreRejectsSchemaAndInvalidCreationWithoutRepair(t *testing.T) {
	for _, mutate := range []string{"PRAGMA user_version=2", "DROP TABLE executions", "DELETE FROM agent_meta"} {
		t.Run(mutate, func(t *testing.T) {
			s, path, key := newTestStore(t)
			id := s.Identity()
			if _, err := s.db.Exec(mutate); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if got, err := Open(context.Background(), path, id, key); !errors.Is(err, ErrSchema) {
				if got != nil {
					got.Close()
				}
				t.Fatalf("want schema rejection: %v", err)
			}
		})
	}
	for _, path := range []string{"", ":memory:", "file:ledger.db?mode=memory", "ledger.db?mode=memory", "\\\\server\\share\\ledger.db"} {
		if s, err := Create(context.Background(), path, testNode, "key", make([]byte, 32)); !errors.Is(err, ErrInput) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("path %q: %v", path, err)
		}
	}
	for _, tc := range []struct {
		node, keyID string
		key         []byte
		want        error
	}{
		{"not-uuid", "key", make([]byte, 32), ErrIdentity},
		{testNode, "", make([]byte, 32), ErrIdentity},
		{testNode, strings.Repeat("k", 129), make([]byte, 32), ErrIdentity},
		{testNode, "key", make([]byte, 33), ErrKey},
	} {
		path := filepath.Join(t.TempDir(), "invalid.db")
		if s, err := Create(context.Background(), path, tc.node, tc.keyID, tc.key); !errors.Is(err, tc.want) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("bad creation: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid creation left a file")
		}
	}
}

func TestStoreSchemaRejectsInvalidDurableFacts(t *testing.T) {
	s, _, _ := newTestStore(t)
	if _, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t))); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE executions SET input_digest=x'01'`,
		`UPDATE executions SET local_fingerprint=x'01'`,
		`UPDATE executions SET policy_revision=-1`,
		`UPDATE executions SET policy_revision=9007199254740992`,
		`UPDATE executions SET policy_revision=1.5`,
		`UPDATE executions SET phase='unknown'`,
		`UPDATE executions SET phase='START_COMMITTED'`,
		`UPDATE executions SET session_id='nonempty'`,
		`UPDATE executions SET created_at='not-a-time'`,
		`UPDATE executions SET auth_rev=9007199254740992`,
		`UPDATE local_claims SET resource_kind='unknown'`,
		`UPDATE local_claims SET operation_id='missing'`,
		`DELETE FROM executions`,
		`UPDATE agent_meta SET schema_version=2`,
		`UPDATE agent_meta SET key_check=x'01'`,
		`UPDATE agent_meta SET id=2`,
	} {
		t.Run(statement, func(t *testing.T) {
			if _, err := s.db.Exec(statement); err == nil {
				t.Fatal("invalid durable fact accepted")
			}
		})
	}
}

func TestStoreFailedCreationRetainsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failed.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Create(ctx, path, testNode, "key", make([]byte, 32)); !errors.Is(err, ErrStorage) {
		t.Fatalf("canceled creation: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("failed creation removed its file", err)
	}
	if _, err := Create(context.Background(), path, testNode, "key", make([]byte, 32)); !errors.Is(err, ErrStorage) {
		t.Fatalf("recreated failed ledger: %v", err)
	}
}

func TestStoreOpenRejectsMissingSchemaConstraints(t *testing.T) {
	s, path, key := newTestStore(t)
	id := s.Identity()
	if _, err := s.db.Exec(`ALTER TABLE local_claims RENAME TO old_claims;
		CREATE TABLE local_claims AS SELECT resource_kind,resource_id,operation_id FROM old_claims;
		DROP TABLE old_claims;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(context.Background(), path, id, key)
	if opened != nil {
		opened.Close()
	}
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("constraint-free schema accepted: %v", err)
	}
}

func TestStoreOpenRejectsUnknownSchemaObjects(t *testing.T) {
	for _, statement := range []string{
		`CREATE TABLE unknown_table(value TEXT)`,
		`CREATE INDEX unknown_index ON executions(stack_id)`,
		`CREATE VIEW unknown_view AS SELECT operation_id FROM executions`,
		`CREATE TRIGGER unknown_trigger AFTER INSERT ON executions BEGIN SELECT 1; END`,
	} {
		t.Run(statement, func(t *testing.T) {
			s, path, key := newTestStore(t)
			id := s.Identity()
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := Open(context.Background(), path, id, key)
			if opened != nil {
				opened.Close()
			}
			if !errors.Is(err, ErrSchema) {
				t.Fatalf("unknown schema object accepted: %v", err)
			}
		})
	}
}

func TestStoreOpenAcceptsVersionOneAcrossCheckoutLineEndings(t *testing.T) {
	for name, expression := range map[string]string{
		"LF":   `replace(sql,char(13)||char(10),char(10))`,
		"CRLF": `replace(replace(sql,char(13)||char(10),char(10)),char(10),char(13)||char(10))`,
	} {
		t.Run(name, func(t *testing.T) {
			s, path, key := newTestStore(t)
			id := s.Identity()
			// Simulate the same version's migration from either checkout format,
			// independently of this test binary's own embedded file line endings.
			if _, err := s.db.Exec(`PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=` + expression + ` WHERE name NOT GLOB 'sqlite_*'; PRAGMA writable_schema=OFF;`); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := Open(context.Background(), path, id, key)
			if err != nil {
				t.Fatalf("same version1 schema with different checkout line endings: %v", err)
			}
			defer opened.Close()
			if _, err := opened.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t))); err != nil {
				t.Fatal(err)
			}
		})
	}
}
