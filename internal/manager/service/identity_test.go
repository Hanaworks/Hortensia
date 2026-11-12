package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"docker-manager/internal/manager/operations"
)

func runtimeConfig(t *testing.T) Config {
	t.Helper()
	raw, _ := json.Marshal(configFixture(t))
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInitializePublishesOneIdentityAndAdmin(t *testing.T) {
	c := runtimeConfig(t)
	ctx := context.Background()
	identity, err := Initialize(ctx, c, "admin", "  example password  ")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(c.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Initialize(ctx, c, "other", "another password"); err == nil {
		t.Fatal("reinitialized existing database")
	}
	after, _ := os.ReadFile(c.DatabaseFile)
	if string(before) != string(after) {
		t.Fatal("repeated init modified database")
	}
	loaded, keys, err := loadIdentity(c)
	if err != nil {
		t.Fatal(err)
	}
	defer clearKeys(&keys)
	if identity != loaded || identity.ManagerID == "" || string(keys.DigestKey) == string(keys.EncryptionKey) {
		t.Fatal("identity not stable or keys not independent")
	}
	s, err := operations.OpenRuntimeService(ctx, c.DatabaseFile, identity.StoreIdentity(), keys)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	login, err := s.Login(ctx, "admin", "  example password  ")
	if err != nil {
		t.Fatal(err)
	}
	var me map[string]any
	if err = json.Unmarshal(login.Reply.Body, &me); err != nil {
		t.Fatal(err)
	}
	if me["auth_rev"] != float64(0) || len(me["system_capabilities"].([]any)) != 1 {
		t.Fatal("bootstrap contract changed")
	}
	if _, err = s.Login(ctx, "admin", "example password"); err == nil {
		t.Fatal("password whitespace removed")
	}
}

func TestInitializePreservesPartialMaterials(t *testing.T) {
	c := runtimeConfig(t)
	if err := ensurePrivateDir(c.KeysDir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.KeysDir, "digest.key")
	if err := writeExclusive(path, []byte("partial")); err != nil {
		t.Fatal(err)
	}
	if _, err := Initialize(context.Background(), c, "admin", "example password"); err == nil {
		t.Fatal("partial initialization overwritten")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "partial" {
		t.Fatal("partial evidence changed")
	}
	if _, err := os.Stat(c.DatabaseFile); !os.IsNotExist(err) {
		t.Fatal("database created beside partial material")
	}
	if _, _, err := loadIdentity(c); err == nil {
		t.Fatal("partial identity accepted")
	}
}

func TestInvalidBootstrapDoesNotPublishMaterial(t *testing.T) {
	for _, input := range [][2]string{{"Admin", "example password"}, {"admin", "short"}, {"admin", string([]byte{255})}} {
		t.Run(input[0], func(t *testing.T) {
			c := runtimeConfig(t)
			if _, err := Initialize(context.Background(), c, input[0], input[1]); err == nil {
				t.Fatal("invalid bootstrap accepted")
			}
			if _, err := os.Stat(c.KeysDir); !os.IsNotExist(err) {
				t.Fatal("invalid input created material")
			}
		})
	}
}

func TestInitializePreservesOrphanSQLiteSidecars(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			c := runtimeConfig(t)
			if err := ensurePrivateDir(filepath.Dir(c.DatabaseFile)); err != nil {
				t.Fatal(err)
			}
			path := c.DatabaseFile + suffix
			original := []byte("original incomplete SQLite material\x00\xff")
			if err := writeExclusive(path, original); err != nil {
				t.Fatal(err)
			}
			if _, err := Initialize(context.Background(), c, "admin", "example password"); err == nil {
				t.Error("orphan SQLite material accepted for initialization")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(original) {
				t.Errorf("original sidecar changed: %v", err)
			}
			for _, newPath := range []string{c.DatabaseFile, filepath.Join(filepath.Dir(c.DatabaseFile), "identity.json"), filepath.Join(c.KeysDir, "digest.key"), filepath.Join(c.KeysDir, "encryption.key")} {
				if _, err := os.Lstat(newPath); !os.IsNotExist(err) {
					t.Errorf("material created beside orphan sidecar: %s", filepath.Base(newPath))
				}
			}
		})
	}
}

func TestInitializeRejectsWindowsMaterialCaseAliasBeforePublication(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows case-insensitive path contract")
	}
	c := runtimeConfig(t)
	c.DatabaseFile = filepath.Join(filepath.Dir(c.DatabaseFile), "IDENTITY.JSON")
	if _, err := Initialize(context.Background(), c, "admin", "example password"); err == nil {
		t.Error("material path alias accepted")
	}
	for _, path := range []string{c.DatabaseFile, filepath.Join(c.KeysDir, "digest.key"), filepath.Join(c.KeysDir, "encryption.key")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("material published for invalid alias: %s", filepath.Base(path))
		}
	}
}
