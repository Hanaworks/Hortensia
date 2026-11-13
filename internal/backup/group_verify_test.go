package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"docker-manager/internal/localstate"
)

// These tests exercise the generic file/manifest contract with a controlled
// inspector and real SQLite copies; they do not claim domain decryption proof.
func completeVerifyFixture(t *testing.T) (GroupRequest, GroupResult, DomainInspector, *sql.DB) {
	t.Helper()
	req, inspect, source := groupFixture(t)
	result, err := CreateGroup(context.Background(), req, inspect)
	if err != nil {
		t.Fatalf("create complete group: %v", err)
	}
	if result.State != StateComplete {
		t.Fatalf("create state = %q", result.State)
	}
	verified, err := VerifyGroup(context.Background(), result.DataManifest, result.SecretManifest, inspect)
	if err != nil || verified.State != StateComplete {
		t.Fatalf("baseline group does not verify: state=%q error=%v", verified.State, err)
	}
	return req, result, inspect, source
}

func verifyReadObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		t.Fatalf("test fixture JSON object: %v", err)
	}
	return object
}

func verifyReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func verifyJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func verifyWriteFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func verifyWriteObject(t *testing.T, path string, object map[string]json.RawMessage) {
	t.Helper()
	verifyWriteFile(t, path, append(verifyJSON(t, object), '\n'))
}

// Rebind only the secret-manifest transport hash, so tests of its contents reach
// secret parsing/cross-link checks rather than merely failing an obsolete hash.
func verifyReplaceSecretManifest(t *testing.T, result GroupResult, raw []byte) {
	t.Helper()
	verifyWriteFile(t, result.SecretManifest, raw)
	data := verifyReadObject(t, verifyReadFile(t, result.DataManifest))
	data["secret_manifest"] = verifyJSON(t, struct {
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	}{len(raw), testHash(raw)})
	verifyWriteObject(t, result.DataManifest, data)
}

func verifyRejected(t *testing.T, result GroupResult, inspect DomainInspector) {
	t.Helper()
	verified, err := VerifyGroup(context.Background(), result.DataManifest, result.SecretManifest, inspect)
	if err == nil {
		t.Fatal("damaged or mismatched backup accepted")
	}
	if verified.State == StateComplete || !reflect.DeepEqual(verified.Report, DomainReport{}) {
		t.Fatalf("failed verification returned a completed/partial proof: %+v", verified)
	}
}

type verifyTreeEntry struct {
	Mode os.FileMode
	Hash string
}

func verifyBackupTree(t *testing.T, roots ...string) map[string]verifyTreeEntry {
	t.Helper()
	entries := map[string]verifyTreeEntry{}
	for index, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			value := verifyTreeEntry{Mode: info.Mode()}
			if !entry.IsDir() {
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				value.Hash = testHash(raw)
			}
			entries[strconv.Itoa(index)+":"+filepath.ToSlash(relative)] = value
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return entries
}

func TestVerifyGroupWithoutSourceDoesNotMutateBackupTree(t *testing.T) {
	req, result, inspect, source := completeVerifyFixture(t)
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(req.Source.DatabaseFile); err != nil {
		t.Fatal(err)
	}
	for _, material := range req.Source.Materials {
		if err := os.Remove(material.Path); err != nil {
			t.Fatal(err)
		}
	}
	// Holding the live deployment runtime lock must not gate offline verify.
	runtimeLock, err := localstate.AcquireLock(filepath.Join(filepath.Dir(req.Source.DatabaseFile), ".agent.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtimeLock.Close(); err != nil {
			t.Error(err)
		}
	})
	before := verifyBackupTree(t, req.DataOut, req.SecretsOut)
	verified, err := VerifyGroup(context.Background(), result.DataManifest, result.SecretManifest, inspect)
	if err != nil || verified.State != StateComplete || verified.BackupID != result.BackupID {
		t.Fatalf("offline verify: result=%+v error=%v", verified, err)
	}
	if after := verifyBackupTree(t, req.DataOut, req.SecretsOut); !reflect.DeepEqual(before, after) {
		t.Fatalf("verify changed backup files/tree: before=%v after=%v", before, after)
	}
}

func TestVerifyGroupRejectsTamperedFilesAndUnexpectedTreeEntries(t *testing.T) {
	for _, kind := range []string{
		"database-bytes", "identity-bytes", "ledger-key-bytes", "journal-key-missing", "config-bytes",
		"extra-data-file", "extra-secret-directory", "temporary-manifest", "wal", "shm", "journal",
	} {
		t.Run(kind, func(t *testing.T) {
			req, result, inspect, _ := completeVerifyFixture(t)
			switch kind {
			case "database-bytes":
				path := filepath.Join(req.DataOut, "sqlite", "state.db")
				raw := verifyReadFile(t, path)
				raw[len(raw)-1] ^= 1
				verifyWriteFile(t, path, raw)
			case "identity-bytes":
				path := filepath.Join(req.SecretsOut, "identity.json")
				verifyWriteFile(t, path, append(verifyReadFile(t, path), ' '))
			case "ledger-key-bytes":
				path := filepath.Join(req.SecretsOut, "keys", "ledger.key")
				raw := verifyReadFile(t, path)
				raw[0] ^= 1
				verifyWriteFile(t, path, raw)
			case "journal-key-missing":
				if err := os.Remove(filepath.Join(req.SecretsOut, "keys", "journal.key")); err != nil {
					t.Fatal(err)
				}
			case "config-bytes":
				path := filepath.Join(req.DataOut, "config.json")
				verifyWriteFile(t, path, append(verifyReadFile(t, path), '\n'))
			case "extra-data-file":
				verifyWriteFile(t, filepath.Join(req.DataOut, "unlisted.txt"), []byte("not in manifest"))
			case "extra-secret-directory":
				if err := os.Mkdir(filepath.Join(req.SecretsOut, "unlisted"), 0700); err != nil {
					t.Fatal(err)
				}
			case "temporary-manifest":
				verifyWriteFile(t, filepath.Join(req.DataOut, ".manifest-leftover.tmp"), []byte("partial"))
			default:
				verifyWriteFile(t, filepath.Join(req.DataOut, "sqlite", "state.db-"+kind), []byte("sidecar"))
			}
			verifyRejected(t, result, inspect)
		})
	}
}

func TestVerifyGroupRejectsCrossGroupSecretManifest(t *testing.T) {
	_, first, inspect, _ := completeVerifyFixture(t)
	_, second, _, _ := completeVerifyFixture(t)
	// Even byte-identical source data and the same controlled domain IDs must
	// not make distinct backup_id groups interchangeable.
	verifyRejected(t, GroupResult{DataManifest: first.DataManifest, SecretManifest: second.SecretManifest}, inspect)
	// Matching the transport hash alone cannot bless another group's identity.
	verifyReplaceSecretManifest(t, first, verifyReadFile(t, second.SecretManifest))
	verifyRejected(t, first, inspect)
}

func TestVerifyGroupRejectsMissingAndNullDataManifestFields(t *testing.T) {
	_, result, inspect, _ := completeVerifyFixture(t)
	original := verifyReadFile(t, result.DataManifest)
	fields := []string{"format", "backup_id", "component", "source_layout", "generation", "identity", "schema_version", "protocol_version", "creator", "sqlite_version", "started_at", "completed_at", "database", "files", "identity_sha256", "keys", "referenced_encryption_key_ids", "inspection", "secret_manifest", "external_materials"}
	for _, field := range fields {
		for _, action := range []string{"missing", "null"} {
			if field == "generation" && action == "null" {
				continue // generation is explicitly nullable, but not optional.
			}
			t.Run(field+"/"+action, func(t *testing.T) {
				object := verifyReadObject(t, original)
				if action == "missing" {
					delete(object, field)
				} else {
					object[field] = json.RawMessage("null")
				}
				verifyWriteObject(t, result.DataManifest, object)
				verifyRejected(t, result, inspect)
			})
		}
	}
}

func TestVerifyGroupRejectsMissingAndNullSecretManifestFields(t *testing.T) {
	_, result, inspect, _ := completeVerifyFixture(t)
	originalData := verifyReadFile(t, result.DataManifest)
	originalSecret := verifyReadFile(t, result.SecretManifest)
	for _, field := range []string{"format", "backup_id", "component", "source_layout", "generation", "identity", "database_sha256", "database_bytes", "identity_sha256", "files"} {
		for _, action := range []string{"missing", "null"} {
			if field == "generation" && action == "null" {
				continue
			}
			t.Run(field+"/"+action, func(t *testing.T) {
				verifyWriteFile(t, result.DataManifest, originalData)
				object := verifyReadObject(t, originalSecret)
				if action == "missing" {
					delete(object, field)
				} else {
					object[field] = json.RawMessage("null")
				}
				verifyReplaceSecretManifest(t, result, append(verifyJSON(t, object), '\n'))
				verifyRejected(t, result, inspect)
			})
		}
	}
}

func TestVerifyGroupRejectsMalformedManifestContents(t *testing.T) {
	_, result, inspect, _ := completeVerifyFixture(t)
	original := verifyReadFile(t, result.DataManifest)
	for _, kind := range []string{"unknown", "duplicate", "non-null-generation", "future-schema", "padded-sha256", "invalid-fingerprint", "missing-table-rows", "null-row-count", "negative-row-count", "overflow-row-count", "duplicate-table", "path-traversal", "absolute-path", "missing-creator-revision", "missing-creator-dirty"} {
		t.Run(kind, func(t *testing.T) {
			object := verifyReadObject(t, original)
			switch kind {
			case "unknown":
				object["unrecognized"] = json.RawMessage("true")
			case "duplicate":
				duplicated := append([]byte(`{"format":"dm-backup-v1",`), bytes.TrimSpace(original)[1:]...)
				verifyWriteFile(t, result.DataManifest, duplicated)
				verifyRejected(t, result, inspect)
				return
			case "non-null-generation":
				object["generation"] = json.RawMessage(`"invented-generation"`)
			case "future-schema":
				object["schema_version"] = json.RawMessage("6")
			case "padded-sha256":
				object["identity_sha256"] = verifyJSON(t, testHash([]byte("identity"))+"=")
			case "path-traversal", "absolute-path":
				database := verifyReadObject(t, object["database"])
				path := "../source.sqlite"
				if kind == "absolute-path" {
					path = result.DataManifest
				}
				database["path"] = verifyJSON(t, path)
				object["database"] = verifyJSON(t, database)
			case "missing-creator-revision", "missing-creator-dirty":
				creator := verifyReadObject(t, object["creator"])
				field := "build_revision"
				if kind == "missing-creator-dirty" {
					field = "build_dirty"
				}
				delete(creator, field)
				object["creator"] = verifyJSON(t, creator)
			default:
				inspection := verifyReadObject(t, object["inspection"])
				switch kind {
				case "invalid-fingerprint":
					inspection["semantic_fingerprint"] = json.RawMessage(`"not-a-32-byte-fingerprint"`)
				case "missing-table-rows":
					delete(inspection, "table_rows")
				case "duplicate-table":
					inspection["table_rows"] = json.RawMessage(`[{"table":"packages","rows":1},{"table":"packages","rows":1}]`)
				default:
					count := "null"
					if kind == "negative-row-count" {
						count = "-1"
					} else if kind == "overflow-row-count" {
						count = "18446744073709551616"
					}
					inspection["table_rows"] = json.RawMessage(`[{"table":"packages","rows":` + count + `}]`)
				}
				object["inspection"] = verifyJSON(t, inspection)
			}
			verifyWriteObject(t, result.DataManifest, object)
			verifyRejected(t, result, inspect)
		})
	}
}

func TestVerifyGroupRejectsChangedDomainReport(t *testing.T) {
	for _, kind := range []string{"table-rows", "sealed-rows", "fingerprint", "node-id", "key-id"} {
		t.Run(kind, func(t *testing.T) {
			_, result, inspect, _ := completeVerifyFixture(t)
			changed := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
				report, err := inspect(ctx, path, set)
				if err != nil {
					return DomainReport{}, err
				}
				switch kind {
				case "table-rows":
					report.TableRows[0].Rows++
				case "sealed-rows":
					report.SealedRows[0].Rows++
					report.ReferencedEncryptionKeyIDs = []string{testJournalKey}
				case "fingerprint":
					report.SemanticFingerprint = testHash([]byte("different-controlled-proof"))
				case "node-id":
					report.Identity.NodeID = "99999999-9999-4999-8999-999999999999"
				case "key-id":
					report.Keys[0].KeyID = "99999999-9999-4999-8999-999999999999"
				}
				return report, nil
			}
			verifyRejected(t, result, changed)
		})
	}
}

func TestVerifyGroupRejectsChangedSealedCountsWithMatchingReferences(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	baseline := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		report, err := inspect(ctx, path, set)
		if err != nil {
			return DomainReport{}, err
		}
		// A controlled report with room for either sealed count keeps both
		// table counts and the referenced-key set valid and unchanged.
		report.TableRows = []RowCount{{Table: "controlled_sealed", Rows: 2}, {Table: "packages", Rows: 1}}
		report.SealedRows = []RowCount{{Table: "controlled_sealed", Rows: 1}}
		report.ReferencedEncryptionKeyIDs = []string{testJournalKey}
		return report, nil
	}
	result, err := CreateGroup(context.Background(), req, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if verified, err := VerifyGroup(context.Background(), result.DataManifest, result.SecretManifest, baseline); err != nil || verified.State != StateComplete {
		t.Fatalf("baseline nonempty sealed-count group: %+v, %v", verified, err)
	}
	changed := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		report, err := baseline(ctx, path, set)
		if err != nil {
			return DomainReport{}, err
		}
		report.SealedRows[0].Rows = 2
		return report, nil
	}
	verifyRejected(t, result, changed)
}

func TestVerifyGroupUsesSavedConfigAndAllMaterialBytes(t *testing.T) {
	req, result, inspect, _ := completeVerifyFixture(t)
	want := map[MaterialRole][]byte{
		RoleIdentity: verifyReadFile(t, filepath.Join(req.SecretsOut, "identity.json")),
		RoleLedger:   verifyReadFile(t, filepath.Join(req.SecretsOut, "keys", "ledger.key")),
		RoleJournal:  verifyReadFile(t, filepath.Join(req.SecretsOut, "keys", "journal.key")),
	}
	wantConfig := verifyReadFile(t, filepath.Join(req.DataOut, "config.json"))
	check := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		if !bytes.Equal(set.ConfigJSON, wantConfig) || len(set.Files) != len(want) {
			return DomainReport{}, errors.New("saved config/material set not forwarded")
		}
		seen := map[MaterialRole]bool{}
		for _, material := range set.Files {
			if seen[material.Role] || !bytes.Equal(material.Raw, want[material.Role]) {
				return DomainReport{}, errors.New("saved material bytes not forwarded")
			}
			seen[material.Role] = true
		}
		return inspect(ctx, path, set)
	}
	before := verifyBackupTree(t, req.DataOut, req.SecretsOut)
	verified, err := VerifyGroup(context.Background(), result.DataManifest, result.SecretManifest, check)
	if err != nil || verified.State != StateComplete {
		t.Fatalf("saved-byte verification failed: %+v, %v", verified, err)
	}
	if after := verifyBackupTree(t, req.DataOut, req.SecretsOut); !reflect.DeepEqual(before, after) {
		t.Fatal("saved-byte verification changed the backup")
	}
}

func TestVerifyGroupRejectsInspectorMutatingBoundDatabase(t *testing.T) {
	_, result, inspect, _ := completeVerifyFixture(t)
	mutating := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		report, err := inspect(ctx, path, set)
		if err != nil {
			return DomainReport{}, err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return DomainReport{}, err
		}
		_, writeErr := file.Write([]byte("changed-after-inspection"))
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return DomainReport{}, err
		}
		return report, nil
	}
	verifyRejected(t, result, mutating)
}

func TestVerifyGroupAllowsNewVerifierSQLiteVersion(t *testing.T) {
	_, result, inspect, _ := completeVerifyFixture(t)
	newRuntime := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		report, err := inspect(ctx, path, set)
		if err != nil {
			return DomainReport{}, err
		}
		report.SQLiteVersion = "different-verification-runtime"
		return report, nil
	}
	verified, err := VerifyGroup(context.Background(), result.DataManifest, result.SecretManifest, newRuntime)
	if err != nil || verified.State != StateComplete || verified.Report.SQLiteVersion != "different-verification-runtime" {
		t.Fatalf("creator SQLite version incorrectly gates verification: %+v, %v", verified, err)
	}
}

func TestVerifyGroupPreservesIntegerCountsBeyondFloatPrecision(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	const exactCount uint64 = 9007199254740993
	largeCount := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		report, err := inspect(ctx, path, set)
		if err != nil {
			return DomainReport{}, err
		}
		report.TableRows[0].Rows = exactCount
		return report, nil
	}
	result, err := CreateGroup(context.Background(), req, largeCount)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyGroup(context.Background(), result.DataManifest, result.SecretManifest, largeCount)
	if err != nil || verified.State != StateComplete || len(verified.Report.TableRows) == 0 || verified.Report.TableRows[0].Rows != exactCount {
		t.Fatalf("integer row count lost precision: %+v, %v", verified, err)
	}
}
