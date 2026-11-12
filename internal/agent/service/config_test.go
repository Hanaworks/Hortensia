package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	d := t.TempDir()
	return Config{ManagerOrigin: "https://localhost", StateDir: filepath.Join(d, "state"), IdentityFile: filepath.Join(d, "identity", "identity.json"), KeysDir: filepath.Join(d, "keys"), DockerCLI: filepath.Join(d, "docker"), DockerSocket: "unix:///var/run/docker.sock", ExecutionTimeoutSeconds: 1800, LogLevel: "info"}
}
func TestConfigRejectsAmbiguity(t *testing.T) {
	c := testConfig(t)
	b, _ := json.Marshal(c)
	if _, e := ParseConfig(b); e != nil {
		t.Fatal(e)
	}
	for _, b := range [][]byte{[]byte(`{"manager_origin":"https://localhost","manager_origin":"https://localhost"}`), []byte(`{"manager_origin":null}`), []byte(`{"unknown":1}`), bytes.Repeat([]byte(" "), 65537)} {
		if _, e := ParseConfig(b); e == nil {
			t.Fatal("ambiguous config accepted")
		}
	}
	c.StateDir = "relative"
	b, _ = json.Marshal(c)
	if _, e := ParseConfig(b); e == nil {
		t.Fatal("relative state accepted")
	}
}
func TestInitializationNeverReplacesEvidence(t *testing.T) {
	c := testConfig(t)
	id, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001")
	if e != nil {
		t.Fatal(e)
	}
	if id.LedgerID == "" || id.CandidateSecret == "" || id.ActiveSecret != "" || id.SchemaVersion != 5 {
		t.Fatal("missing candidate")
	}
	before, _ := os.ReadFile(c.IdentityFile)
	if _, e = Initialize(context.Background(), c, id.NodeID); e == nil {
		t.Fatal("overwrote evidence")
	}
	after, _ := os.ReadFile(c.IdentityFile)
	if !bytes.Equal(before, after) {
		t.Fatal("identity changed")
	}
	loaded, k, e := loadIdentity(c)
	defer clearKeys(&k)
	if e != nil || loaded.LedgerID != id.LedgerID {
		t.Fatal("reload", e)
	}
}
func TestPartialInitializationIsNotRepaired(t *testing.T) {
	c := testConfig(t)
	if e := os.MkdirAll(c.KeysDir, 0700); e != nil {
		t.Fatal(e)
	}
	key := filepath.Join(c.KeysDir, "ledger.key")
	original := bytes.Repeat([]byte{9}, 32)
	if e := os.WriteFile(key, original, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001"); e == nil {
		t.Fatal("partial initialization overwritten")
	}
	after, _ := os.ReadFile(key)
	if !bytes.Equal(after, original) {
		t.Fatal("original key replaced")
	}
	if _, e := os.Stat(c.IdentityFile); !os.IsNotExist(e) {
		t.Fatal("partial init created identity")
	}
}
func TestIdentityRejectsCaseAliasesAndNullFields(t *testing.T) {
	c := testConfig(t)
	if _, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001"); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(c.IdentityFile)
	for _, mutated := range [][]byte{bytes.Replace(raw, []byte(`"node_id"`), []byte(`"NODE_ID"`), 1), bytes.Replace(raw, []byte(`"credential_generation":0`), []byte(`"credential_generation":null`), 1)} {
		if e := os.WriteFile(c.IdentityFile, mutated, 0600); e != nil {
			t.Fatal(e)
		}
		_, k, e := loadIdentity(c)
		clearKeys(&k)
		if e != ErrIdentity {
			t.Fatal("accepted ambiguous identity", e)
		}
	}
}
