package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const testNode = "55555555-5555-4555-8555-555555555555"
const testLedger = "66666666-6666-4666-8666-666666666666"
const testLedgerKey = "77777777-7777-4777-8777-777777777777"
const testJournalKey = "88888888-8888-4888-8888-888888888888"

func testHash(raw []byte) string {
	h := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func groupFixture(t *testing.T) (GroupRequest, DomainInspector, *sql.DB) {
	t.Helper()
	db, source, root := fixture(t, true)
	identity := []byte(`{"private":"candidate-secret-that-must-remain-private"}`)
	material := []MaterialSource{}
	for _, entry := range []struct {
		role MaterialRole
		raw  []byte
	}{{RoleIdentity, identity}, {RoleLedger, bytes.Repeat([]byte{0x71}, 32)}, {RoleJournal, bytes.Repeat([]byte{0x92}, 32)}} {
		p := filepath.Join(root, string(entry.role)+".source")
		if err := os.WriteFile(p, entry.raw, 0600); err != nil {
			t.Fatal(err)
		}
		material = append(material, MaterialSource{Role: entry.role, Path: p})
	}
	config := []byte(`{"format":"dm-backup-config-v1","component":"agent","supplied_config":true,"manager_origin":"https://manager.example","docker_socket":"unix:///var/run/docker.sock","execution_timeout_seconds":1800,"log_level":"info","trust_roots_mode":"system"}`)
	ext := []ExternalMaterial{{"docker_compose_cli", true, "provision_supported_version"}, {"engine_endpoint", true, "revalidate_endpoint"}, {"tls_trust_roots", true, "provision_current"}, {"agent_workspaces", true, "preserve_and_reconcile"}, {"agent_operations", true, "preserve_and_reconcile"}, {"docker_images", false, "excluded"}, {"docker_data_root", false, "excluded"}, {"application_volumes_and_binds", false, "excluded"}, {"external_databases", false, "excluded"}}
	request := GroupRequest{Source: Source{Component: ComponentAgent, DatabaseFile: source, MaterialsLockFile: filepath.Join(root, ".materials.lock"), Materials: material, ConfigJSON: config, Creator: BuildInfo{ApplicationVersion: "generic-test"}, ExternalMaterials: ext}, DataOut: filepath.Join(root, "data"), SecretsOut: filepath.Join(root, "secrets")}
	inspect := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		if !bytes.Equal(set.ConfigJSON, config) || len(set.Files) != 3 {
			return DomainReport{}, errors.New("controlled callback received wrong config/materials")
		}
		conn, e := sql.Open("sqlite", testURI(path, "ro")+"&immutable=1")
		if e != nil {
			return DomainReport{}, e
		}
		defer conn.Close()
		var count uint64
		if e = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM packages").Scan(&count); e != nil {
			return DomainReport{}, e
		}
		return DomainReport{Component: ComponentAgent, Identity: IdentityRef{NodeID: testNode, LedgerID: testLedger}, SchemaVersion: 5, ProtocolVersion: 1, SQLiteVersion: "controlled-runtime", Keys: []KeyRef{{Role: RoleJournal, KeyID: testJournalKey}, {Role: RoleLedger, KeyID: testLedgerKey}}, ReferencedEncryptionKeyIDs: []string{}, TableRows: []RowCount{{Table: "controlled_sealed", Rows: 0}, {Table: "packages", Rows: count}}, SealedRows: []RowCount{{Table: "controlled_sealed", Rows: 0}}, SemanticFormat: "dm-agent-runtime-semantic-v1", SemanticFingerprint: testHash([]byte("controlled-semantic-proof"))}, nil
	}
	return request, inspect, db
}

func TestCreateGroupBindsRealSQLiteAndSecretBytes(t *testing.T) {
	req, inspect, db := groupFixture(t)
	execSQL(t, db, "UPDATE packages SET version=17")
	result, err := CreateGroup(context.Background(), req, inspect)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateComplete || result.BackupID == "" {
		t.Fatalf("result=%+v", result)
	}
	copyDB, err := sql.Open("sqlite", testURI(filepath.Join(req.DataOut, "sqlite", "state.db"), "ro")+"&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	var copiedVersion int
	readErr := copyDB.QueryRow("SELECT version FROM packages WHERE id=1").Scan(&copiedVersion)
	closeErr := copyDB.Close()
	if readErr != nil || closeErr != nil || copiedVersion != 17 {
		t.Fatalf("live WAL commit not copied: version=%d read=%v close=%v", copiedVersion, readErr, closeErr)
	}
	data, err := os.ReadFile(result.DataManifest)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(result.SecretManifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if json.Unmarshal(data, &manifest) != nil {
		t.Fatal("bad manifest JSON")
	}
	var secretLink struct {
		Bytes int64  `json:"bytes"`
		SHA   string `json:"sha256"`
	}
	if json.Unmarshal(manifest["secret_manifest"], &secretLink) != nil || secretLink.Bytes != int64(len(secret)) || secretLink.SHA != testHash(secret) {
		t.Fatal("secret link does not hash exact bytes")
	}
	if bytes.Contains(secret, []byte("candidate-secret-that-must-remain-private")) || bytes.Contains(data, []byte("candidate-secret-that-must-remain-private")) {
		t.Fatal("manifest leaked identity secret")
	}
	identity, _ := os.ReadFile(filepath.Join(req.SecretsOut, "identity.json"))
	want, _ := os.ReadFile(req.Source.Materials[0].Path)
	if !bytes.Equal(identity, want) {
		t.Fatal("identity bytes were not retained")
	}
	verified, err := VerifyGroup(context.Background(), result.DataManifest, result.SecretManifest, inspect)
	if err != nil {
		t.Fatal(err)
	}
	if verified.State != StateComplete || !reflect.DeepEqual(verified.Report, result.Report) {
		t.Fatalf("verified mismatch: %+v", verified)
	}
	assertBoot(t, db)
}

func TestGroupPathRejectsUnsafeInputsBeforeCreatingOutput(t *testing.T) {
	for _, kind := range []string{"nil-context", "same-roots", "nested-roots", "wrong-lock", "material-collision", "short-key", "bad-config"} {
		t.Run(kind, func(t *testing.T) {
			req, inspect, _ := groupFixture(t)
			ctx := context.Background()
			switch kind {
			case "nil-context":
				ctx = nil
			case "same-roots":
				req.SecretsOut = req.DataOut
			case "nested-roots":
				req.SecretsOut = filepath.Join(req.DataOut, "nested")
			case "wrong-lock":
				req.Source.MaterialsLockFile = filepath.Join(filepath.Dir(req.Source.DatabaseFile), "wrong.lock")
			case "material-collision":
				req.Source.Materials[0].Path = req.Source.DatabaseFile
			case "short-key":
				if e := os.WriteFile(req.Source.Materials[1].Path, []byte("short"), 0600); e != nil {
					t.Fatal(e)
				}
			case "bad-config":
				req.Source.ConfigJSON = []byte(`{"component":"agent"}`)
			}
			if _, e := CreateGroup(ctx, req, inspect); e == nil {
				t.Fatal("unsafe input accepted")
			}
			if _, e := os.Lstat(req.DataOut); !os.IsNotExist(e) {
				t.Fatalf("invalid input created output: %v", e)
			}
		})
	}
}

func TestCreateGroupAlreadyCancelledHasNoOutput(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := CreateGroup(ctx, req, inspect)
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel=%v", e)
	}
	if _, e = os.Lstat(req.DataOut); !os.IsNotExist(e) {
		t.Fatal("created output")
	}
}

func TestGroupPublishDoesNotOverwriteExistingFile(t *testing.T) {
	root := t.TempDir()
	if e := os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	old, newName := filepath.Join(root, "temporary"), filepath.Join(root, "manifest.json")
	if e := os.WriteFile(old, []byte("candidate"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(newName, []byte("previous"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := groupRenameNoReplace(old, newName); e == nil {
		t.Fatal("replaced prior manifest")
	}
	raw, _ := os.ReadFile(newName)
	if string(raw) != "previous" {
		t.Fatal("old manifest changed")
	}
}

func TestCreateGroupRejectsReplacedSourceMaterialIdentity(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	wrapped := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		raw, e := os.ReadFile(req.Source.Materials[0].Path)
		if e != nil {
			return DomainReport{}, e
		}
		temporary := req.Source.Materials[0].Path + ".replacement"
		if e = os.WriteFile(temporary, raw, 0600); e != nil {
			return DomainReport{}, e
		}
		if e = os.Rename(temporary, req.Source.Materials[0].Path); e != nil {
			return DomainReport{}, e
		}
		return inspect(ctx, path, set)
	}
	result, e := CreateGroup(context.Background(), req, wrapped)
	if !errors.Is(e, ErrMaterialsChanged) || result.State != StatePartial {
		t.Fatalf("replaced source=%+v %v", result, e)
	}
	if _, e = os.Lstat(filepath.Join(req.DataOut, "manifest.json")); !os.IsNotExist(e) {
		t.Fatal("published after material replacement")
	}
}

func TestGroupReportRejectsSealedCountOutsideTableTotal(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	wrapped := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		r, e := inspect(ctx, path, set)
		r.SealedRows[0].Rows = 1
		r.ReferencedEncryptionKeyIDs = []string{testJournalKey}
		return r, e
	}
	result, e := CreateGroup(context.Background(), req, wrapped)
	if !errors.Is(e, ErrVerification) || result.State != StatePartial {
		t.Fatalf("inconsistent count=%+v %v", result, e)
	}
}

func TestGroupRejectsHardlinkedMaterialAndLock(t *testing.T) {
	for _, kind := range []string{"material", "lock"} {
		t.Run(kind, func(t *testing.T) {
			req, inspect, _ := groupFixture(t)
			path := req.Source.Materials[0].Path
			if kind == "lock" {
				path = req.Source.MaterialsLockFile
				if e := os.WriteFile(path, []byte{}, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if e := os.Link(path, path+".hardlink"); e != nil {
				t.Fatal(e)
			}
			if _, e := CreateGroup(context.Background(), req, inspect); e == nil {
				t.Fatal("hardlink accepted")
			}
			if _, e := os.Lstat(req.DataOut); !os.IsNotExist(e) {
				t.Fatal("hardlink wrote output")
			}
		})
	}
}
