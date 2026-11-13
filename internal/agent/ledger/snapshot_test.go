package ledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSnapshotInspectionLegalRuntimeHistory(t *testing.T) {
	for _, stage := range []string{"preparing", "ready", "ready_failed_finalized", "not_attempted", "created", "exited", "preparation_failed", "start_failed", "unknown", "interrupted", "incomplete", "finalized", "finalized_quarantined"} {
		t.Run(stage, func(t *testing.T) {
			s, path, id, key, journal := closedInspectionStore(t)
			ctx := context.Background()
			r := runtimePrepare(t, s)
			switch stage {
			case "ready", "ready_failed_finalized":
				if _, err := s.CommitPrepared(ctx, r.OperationID, runtimeEvidence(t, s, r)); err != nil {
					t.Fatal(err)
				}
				if stage == "ready_failed_finalized" {
					event, err := s.RecordPreparationFailure(ctx, r.OperationID, Failure{Code: "preflight_failed", BootID: s.BootID()})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = s.Finalize(ctx, releaseRequest(t, r, event.Seq, "failed")); err != nil {
						t.Fatal(err)
					}
				}
			case "preparation_failed":
				if _, err := s.RecordPreparationFailure(ctx, r.OperationID, Failure{Code: "preflight_failed", BootID: s.BootID()}); err != nil {
					t.Fatal(err)
				}
			case "preparing":
			default:
				runtimeStart(t, s, r, nil)
				if stage == "not_attempted" {
					break
				}
				if stage == "start_failed" {
					if _, err := s.RecordStartFailure(ctx, r.OperationID, Failure{Code: "spawn_failed", BootID: s.BootID()}); err != nil {
						t.Fatal(err)
					}
					break
				}
				if err := s.RecordProcessCreated(ctx, r.OperationID, 42); err != nil {
					t.Fatal(err)
				}
				if stage == "created" {
					break
				}
				zero := 0
				fact := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli()), Output: []byte("private-output")}
				if stage == "unknown" {
					fact.Outcome = "unknown"
					fact.ExitCode = nil
				}
				if stage == "interrupted" {
					fact.Outcome = "interrupted"
					fact.InterruptReason = "canceled"
				}
				if err := s.RecordProcessExit(ctx, r.OperationID, fact); err != nil {
					t.Fatal(err)
				}
				if stage == "exited" {
					break
				}
				observation := runtimeObservation(t, r, "complete")
				if stage == "unknown" || stage == "interrupted" {
					observation = ObservationEvidence{}
				}
				if stage == "incomplete" {
					observation = runtimeObservation(t, r, "incomplete")
				}
				event, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, observation)
				if err != nil {
					t.Fatal(err)
				}
				if stage == "finalized" || stage == "finalized_quarantined" {
					if _, err = s.Finalize(ctx, releaseRequest(t, r, event.Seq, "succeeded")); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "finalized_quarantined" {
					fact.Output = []byte("conflicting private exit")
					if err = s.RecordProcessExit(ctx, r.OperationID, fact); !errors.Is(err, ErrConflict) {
						t.Fatal(err)
					}
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			report, err := InspectRuntimeSnapshot(ctx, path, id, key, journal)
			if err != nil {
				t.Fatalf("legal %s rejected: %v", stage, err)
			}
			if report.TableRows["executions"] != 1 || report.SealedRows["runtime_documents"] == 0 {
				t.Fatalf("history omitted: %+v", report)
			}
		})
	}
}

func TestSnapshotInspectionRejectsAllSealedKindsAndMetadata(t *testing.T) {
	mutations := []string{
		`UPDATE runtime_documents SET body_cipher=zeroblob(length(body_cipher)) WHERE kind='input'`,
		`UPDATE runtime_documents SET wrapped_dek=zeroblob(60) WHERE kind='preparation'`,
		`UPDATE runtime_documents SET body_fingerprint=zeroblob(32) WHERE kind='process'`,
		`UPDATE runtime_documents SET body_cipher=zeroblob(length(body_cipher)) WHERE kind='candidate'`,
		`UPDATE execution_events SET body_cipher=zeroblob(length(body_cipher)) WHERE event_seq=1`,
		`UPDATE execution_events SET wrapped_dek=zeroblob(60) WHERE kind='result'`,
		`UPDATE finalizations SET body_fingerprint=zeroblob(32)`,
		`UPDATE event_keys SET uses=0`,
		`UPDATE project_bindings SET ownership_revision=ownership_revision+1`,
		`UPDATE process_facts SET owner_boot='private-secret-owner'`,
		`UPDATE process_facts SET spawn_state='created'`,
		`UPDATE process_facts SET process_id=NULL`,
		`UPDATE process_facts SET cancel_reason='canceled'`,
		`UPDATE executions SET local_fingerprint=zeroblob(32)`,
		`DELETE FROM execution_events WHERE event_seq=1`,
		`INSERT INTO local_claims(resource_kind,resource_id,operation_id) SELECT 'stack',stack_id,operation_id FROM executions`,
		`CREATE TABLE private_secret_table(value TEXT)`,
		`CREATE TRIGGER private_secret_trigger AFTER INSERT ON executions BEGIN SELECT 1; END`,
		`DELETE FROM event_state`,
	}
	for _, mutation := range mutations {
		t.Run(mutation, func(t *testing.T) {
			s, path, id, key, journal := closedInspectionStore(t)
			r, event := runtimeReviewCandidate(t, s, "")
			if _, err := s.Finalize(context.Background(), releaseRequest(t, r, event.Seq, "succeeded")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			s.Close()
			report, err := InspectRuntimeSnapshot(context.Background(), path, id, key, journal)
			if err == nil || report.SemanticFingerprint != "" {
				t.Fatalf("damaged evidence accepted: %+v %v", report, err)
			}
			if bytes.Contains([]byte(err.Error()), []byte("private-secret")) {
				t.Fatal("secret error leaked")
			}
		})
	}
}

func TestSnapshotInspectionRejectsAuthenticatedOrphanAndInputMismatch(t *testing.T) {
	for _, kind := range []string{"orphan-preparation", "input-metadata", "private-unknown-field", "missing-claim", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			s, path, id, key, journal := closedInspectionStore(t)
			ctx := context.Background()
			r := runtimePrepare(t, s)
			switch kind {
			case "orphan-preparation":
				sealed, err := s.sealDocument(ctx, r, "preparation", 0, runtimeEvidence(t, s, r))
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.db.Exec(`INSERT INTO runtime_documents(operation_id,kind,seq,body_cipher,wrapped_dek,key_id,body_fingerprint) VALUES(?,?,?,?,?,?,?)`, r.OperationID, "preparation", 0, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint)
				if err != nil {
					t.Fatal(err)
				}
			case "input-metadata":
				if _, err := s.db.Exec(`UPDATE executions SET project_name='different'`); err != nil {
					t.Fatal(err)
				}
			case "missing-claim":
				if _, err := s.db.Exec(`DELETE FROM local_claims WHERE resource_kind='stack'`); err != nil {
					t.Fatal(err)
				}
			case "private-unknown-field":
				raw, _ := json.Marshal(runtimeEvidence(t, s, r))
				raw = append(raw[:len(raw)-1], []byte(`,"UnknownPrivateField":true}`)...)
				sealed, err := s.sealEvent(ctx, eventBinding{r.OperationID, r.Digest, "runtime-preparation", 0}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.Exec(`INSERT INTO runtime_documents(operation_id,kind,seq,body_cipher,wrapped_dek,key_id,body_fingerprint) VALUES(?,?,?,?,?,?,?)`, r.OperationID, "preparation", 0, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if _, err := s.db.Exec(`UPDATE runtime_documents SET body_cipher=zeroblob(?)`, inspectionBodyLimit+29); err != nil {
					t.Fatal(err)
				}
			}
			s.Close()
			_, err := InspectRuntimeSnapshot(ctx, path, id, key, journal)
			if err == nil {
				t.Fatal("authenticated damaged shape accepted")
			}
			if kind == "oversize" && !errors.Is(err, ErrInspectionLimit) {
				t.Fatalf("budget misclassified: %v", err)
			}
		})
	}
}

func TestSnapshotInspectionHistoricalScansQueriesAndTail(t *testing.T) {
	for _, damage := range []string{"valid", "member-tail", "member-ordinal", "member-group", "member-count", "member-phase", "member-ack", "query-tail", "query-missing", "historical-event-tail"} {
		t.Run(damage, func(t *testing.T) {
			s, path, id, key, journal := closedInspectionStore(t)
			ctx := context.Background()
			r, first := runtimeReviewCandidate(t, s, "", func(r Record) ObservationEvidence { return runtimeObservation(t, r, "incomplete") })
			page := rootPage(t, s, rootScanRequest(t, "all", "", "", 200))
			if page.Total != 1 {
				t.Fatal(page.Total)
			}
			if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, first.Seq, rootScanSession); err != nil {
				t.Fatal(err)
			}
			queryID := scanContractID(971)
			if _, err := s.BeginObservationQuery(ctx, r.OperationID, queryID, 200); err != nil {
				t.Fatal(err)
			}
			last, err := s.RecordObservationUpdate(ctx, r.OperationID, queryID, runtimeObservation(t, r, "complete"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Finalize(ctx, releaseRequest(t, r, last.Seq, "succeeded")); err != nil {
				t.Fatal(err)
			}
			// An old NULL/expired query is retained even after finalization.
			if _, err = s.db.Exec(`INSERT INTO observation_queries(query_id,operation_id,owner_boot,page_limit,created_at) VALUES(?,?,?,?,?)`, scanContractID(972), r.OperationID, scanContractID(973), 200, 0); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "member-tail":
				_, err = s.db.Exec(`UPDATE recovery_members SET record_fingerprint=zeroblob(32) WHERE scan_id=? AND ordinal=0`, page.ScanID)
			case "member-ordinal":
				_, err = s.db.Exec(`UPDATE recovery_members SET ordinal=ordinal+1 WHERE scan_id=?`, page.ScanID)
			case "member-group":
				_, err = s.db.Exec(`UPDATE recovery_members SET scan_id=? WHERE scan_id=?`, scanContractID(1991), page.ScanID)
			case "member-count":
				_, err = s.db.Exec(`UPDATE recovery_control SET active_total=active_total+1`)
			case "member-phase":
				_, err = s.db.Exec(`UPDATE recovery_members SET phase='ready' WHERE scan_id=?`, page.ScanID)
			case "member-ack":
				_, err = s.db.Exec(`UPDATE recovery_members SET report_acked=1 WHERE scan_id=?`, page.ScanID)
			case "query-tail":
				_, err = s.db.Exec(`UPDATE observation_queries SET result_seq=2 WHERE query_id=?`, queryID)
			case "query-missing":
				_, err = s.db.Exec(`DELETE FROM observation_queries WHERE query_id=?`, queryID)
			case "historical-event-tail":
				_, err = s.db.Exec(`UPDATE execution_events SET body_cipher=zeroblob(length(body_cipher)) WHERE operation_id=? AND event_seq=2`, r.OperationID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenRuntime(ctx, path, id, key, journal, RuntimeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err = reopened.Close(); err != nil {
				t.Fatal(err)
			}
			report, err := InspectRuntimeSnapshot(ctx, path, id, key, journal)
			if damage == "valid" {
				if err != nil {
					t.Fatalf("old scan/ACK/finalized/boot/query rejected: %v", err)
				}
				if report.TableRows["observation_queries"] != 2 || report.SealedRows["execution_events"] != 3 {
					t.Fatal("history omitted")
				}
			} else if err == nil {
				t.Fatalf("damaged tail accepted: %+v", report)
			}
		})
	}
}

func TestSnapshotInspectionHistoricalOwnershipAndProfile(t *testing.T) {
	for _, damage := range []bool{false, true} {
		t.Run(fmt.Sprint(damage), func(t *testing.T) {
			s, path, id, key, journal := closedInspectionStore(t)
			ctx := context.Background()
			first, e := runtimeReviewCandidate(t, s, "")
			if _, err := s.Finalize(ctx, releaseRequest(t, first, e.Seq, "succeeded")); err != nil {
				t.Fatal(err)
			}
			second, e2 := runtimeReviewCandidate(t, s, scanContractID(980))
			if _, err := s.Finalize(ctx, releaseRequest(t, second, e2.Seq, "succeeded")); err != nil {
				t.Fatal(err)
			}
			p, _ := s.RuntimeProfile(ctx)
			p.PolicyRevision++
			p.ComposeVersion = "new-compose"
			p.Fingerprint = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{23}, 32))
			if err := s.BindRuntimeProfile(ctx, p); err != nil {
				t.Fatal(err)
			}
			if damage {
				if _, err := s.db.Exec(`UPDATE project_bindings SET ownership_revision=1,last_operation_id=?,baseline_seq=?`, first.OperationID, e.Seq); err != nil {
					t.Fatal(err)
				}
			}
			s.Close()
			_, err := InspectRuntimeSnapshot(ctx, path, id, key, journal)
			if damage && err == nil {
				t.Fatal("old authenticated head accepted")
			}
			if !damage && err != nil {
				t.Fatalf("historical prep compared to current head/profile: %v", err)
			}
		})
	}
}

func TestSnapshotInspectionEmptyUnboundProfile(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	jk := JournalKey{ID: "empty-profile-key", Key: bytes.Repeat([]byte{2}, 32)}
	path := filepath.Join(t.TempDir(), "empty.db")
	s, err := CreateRuntime(context.Background(), path, testNode, "ledger-key", key, jk, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Identity()
	s.Close()
	if _, err = InspectRuntimeSnapshot(context.Background(), path, id, key, jk); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotInspectionRekeyPreservesSemanticRoot(t *testing.T) {
	s, path, id, key, journal := closedInspectionStore(t)
	r, event := runtimeReviewCandidate(t, s, "")
	if _, err := s.Finalize(context.Background(), releaseRequest(t, r, event.Seq, "succeeded")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := InspectRuntimeSnapshot(context.Background(), path, id, key, journal)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", strings.Replace(databaseURI(path), "foreign_keys%28ON%29", "foreign_keys%28OFF%29", 1))
	if err != nil {
		t.Fatal(err)
	}
	old := &Store{identity: id, key: append([]byte(nil), key...), journal: &journalConfig{keyID: journal.ID, kek: append([]byte(nil), journal.Key...)}}
	defer old.clearOwnedKeys()
	newKey := JournalKey{ID: "new-test-kek", Key: bytes.Repeat([]byte{93}, 32)}
	newReader := &Store{identity: id, key: append([]byte(nil), key...), journal: &journalConfig{keyID: newKey.ID, kek: append([]byte(nil), newKey.Key...)}}
	defer newReader.clearOwnedKeys()
	for _, table := range []string{"execution_events", "runtime_documents", "finalizations"} {
		cols, err := inspectionColumns(context.Background(), db, table)
		if err != nil {
			t.Fatal(err)
		}
		err = inspectionWalk(context.Background(), db, table, cols, func(v []any) error {
			m := map[string]any{}
			for i, c := range cols {
				m[c.name] = v[i]
			}
			record, err := old.readRecord(context.Background(), db, m["operation_id"].(string))
			if err != nil {
				return err
			}
			kind := "finalized"
			var seq uint64
			switch table {
			case "execution_events":
				kind = m["kind"].(string)
				seq = uint64(m["event_seq"].(int64))
			case "runtime_documents":
				kind = "runtime-" + m["kind"].(string)
				seq = uint64(m["seq"].(int64))
			case "finalizations":
				seq = uint64(m["result_event_seq"].(int64))
			}
			binding := eventBinding{record.OperationID, record.Digest, kind, seq}
			raw, err := old.openEvent(binding, sealedEvent{Cipher: m["body_cipher"].([]byte), WrappedDEK: m["wrapped_dek"].([]byte), KeyID: m["key_id"].(string), Fingerprint: m["body_fingerprint"].([]byte)})
			if err != nil {
				return err
			}
			defer clear(raw)
			dek := make([]byte, 32)
			defer clear(dek)
			if _, err = rand.Read(dek); err != nil {
				return err
			}
			bodyAEAD, _ := eventAEAD(dek)
			wrapAEAD, _ := eventAEAD(newKey.Key)
			cipher := bodyAEAD.Seal(nil, nil, raw, newReader.eventAAD("dm-event-body-v1", binding, newKey.ID))
			wrapped := wrapAEAD.Seal(nil, nil, dek, newReader.eventAAD("dm-event-dek-v1", binding, newKey.ID))
			query := `UPDATE ` + table + ` SET body_cipher=?,wrapped_dek=?,key_id=? WHERE operation_id=?`
			args := []any{cipher, wrapped, newKey.ID, record.OperationID}
			if table == "execution_events" {
				query += ` AND event_seq=?`
				args = append(args, seq)
			}
			if table == "runtime_documents" {
				query += ` AND kind=? AND seq=?`
				args = append(args, m["kind"], seq)
			}
			_, err = db.Exec(query, args...)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`UPDATE event_keys SET key_id=?,key_check=?,uses=1048576 WHERE id=1`, newKey.ID, journalKeyCheck(id, newKey)); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := InspectRuntimeSnapshot(context.Background(), path, id, key, newKey)
	if err != nil {
		t.Fatal(err)
	}
	if before.SemanticFingerprint != after.SemanticFingerprint || !reflect.DeepEqual(before.TableRows, after.TableRows) || !reflect.DeepEqual(before.SealedRows, after.SealedRows) {
		t.Fatal("new KEK altered semantic root")
	}
	db, err = sql.Open("sqlite", databaseURI(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE executions SET created_at=created_at-1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	changed, err := InspectRuntimeSnapshot(context.Background(), path, id, key, newKey)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SemanticFingerprint == after.SemanticFingerprint {
		t.Fatal("ordinary metadata absent from semantic root")
	}
}

func TestSnapshotInspectionRejectsInconsistentControlAndProfile(t *testing.T) {
	for _, mutation := range []string{`UPDATE recovery_control SET current_session=''`, `UPDATE runtime_profile SET engine_id=''`, `UPDATE runtime_profile SET fingerprint=CAST(x'ff' AS TEXT)`} {
		t.Run(mutation, func(t *testing.T) {
			s, path, id, key, journal := closedInspectionStore(t)
			runtimePrepare(t, s)
			if _, err := s.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if _, err := InspectRuntimeSnapshot(context.Background(), path, id, key, journal); err == nil {
				t.Fatal("inconsistent metadata accepted")
			}
		})
	}
}

func TestSnapshotInspectionSealedHistoryAndTail(t *testing.T) {
	for _, table := range []string{"execution_events", "finalizations", "runtime_documents"} {
		for _, which := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s-%d", table, which), func(t *testing.T) {
				s, path, id, key, journal := closedInspectionStore(t)
				ctx := context.Background()
				operations := []string{scanContractID(1900), scanContractID(1901)}
				for _, operation := range operations {
					r, event := runtimeReviewCandidate(t, s, operation)
					if _, err := s.Finalize(ctx, releaseRequest(t, r, event.Seq, "succeeded")); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.db.Exec(`UPDATE `+table+` SET body_cipher=zeroblob(length(body_cipher)) WHERE operation_id=?`, operations[which]); err != nil {
					t.Fatal(err)
				}
				s.Close()
				if _, err := InspectRuntimeSnapshot(ctx, path, id, key, journal); err == nil {
					t.Fatal("corrupt history/tail omitted")
				}
			})
		}
	}
}

func TestSnapshotInspectionOrdinaryRowBudgetAndDeadline(t *testing.T) {
	s, path, id, key, journal := closedInspectionStore(t)
	if _, err := s.db.Exec(`UPDATE runtime_profile SET fingerprint=CAST(zeroblob(?) AS TEXT)`, inspectionRowLimit+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := InspectRuntimeSnapshot(context.Background(), path, id, key, journal); !errors.Is(err, ErrInspectionLimit) {
		t.Fatalf("ordinary row budget misclassified: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if _, err := InspectRuntimeSnapshot(ctx, path, id, key, journal); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline lost: %v", err)
	}
}

func TestSnapshotInspectionRejectsLegacyAndSidecar(t *testing.T) {
	s, path, key, journal := releaseStore(t)
	id := s.Identity()
	s.Close()
	if _, err := InspectRuntimeSnapshot(context.Background(), path, id, key, journal); !errors.Is(err, ErrSchema) {
		t.Fatalf("legacy accepted: %v", err)
	}
	runtime, runtimePath, runtimeID, runtimeKey, runtimeJournal := closedInspectionStore(t)
	runtime.Close()
	if err := os.WriteFile(runtimePath+"-wal", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRuntimeSnapshot(context.Background(), runtimePath, runtimeID, runtimeKey, runtimeJournal); !errors.Is(err, ErrInput) {
		t.Fatalf("sidecar ignored: %v", err)
	}
}

func closedInspectionStore(t *testing.T) (*Store, string, Identity, []byte, JournalKey) {
	t.Helper()
	s, path, key, journal := runtimeStore(t)
	id := s.Identity()
	return s, path, id, key, journal
}

func TestSnapshotInspectionEmptyAndNoMutation(t *testing.T) {
	s, path, id, key, journal := closedInspectionStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := InspectRuntimeSnapshot(context.Background(), path, id, key, journal)
	if err != nil {
		t.Fatalf("empty v5 rejected: %v", err)
	}
	if r.Identity != id || r.SchemaVersion != 5 || r.ProtocolVersion != 1 || r.EncryptionKeyID != journal.ID || len(r.TableRows) != 17 || len(r.SealedRows) != 3 || r.SemanticFormat != "dm-agent-runtime-semantic-v1" || !validDigest(r.SemanticFingerprint) {
		t.Fatalf("incomplete report: %+v", r)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("snapshot changed")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("sidecar %s: %v", suffix, err)
		}
	}
	r2, err := InspectRuntimeSnapshot(context.Background(), path, id, key, journal)
	if err != nil || r.SemanticFingerprint != r2.SemanticFingerprint {
		t.Fatalf("unstable fingerprint: %v", err)
	}
}

func TestSnapshotInspectionWrongKeysAndCanceled(t *testing.T) {
	s, path, id, key, journal := closedInspectionStore(t)
	s.Close()
	for _, tc := range []struct {
		name    string
		id      Identity
		key     []byte
		journal JournalKey
		want    error
	}{
		{"identity", Identity{NodeID: scanContractID(989), LedgerID: id.LedgerID, KeyID: id.KeyID}, key, journal, ErrIdentity},
		{"ledger-key", id, bytes.Repeat([]byte{99}, 32), journal, ErrKey},
		{"journal-key", id, key, JournalKey{ID: journal.ID, Key: bytes.Repeat([]byte{98}, 32)}, ErrKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := InspectRuntimeSnapshot(context.Background(), path, tc.id, tc.key, tc.journal)
			if !errors.Is(err, tc.want) || got.SemanticFingerprint != "" {
				t.Fatalf("wrong classification/report: %+v %v", got, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectRuntimeSnapshot(ctx, path, id, key, journal); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
