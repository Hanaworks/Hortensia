package ledger

import (
	"bytes"
	"context"
	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeFormatDoesNotAcceptFixtures(t *testing.T) {
	ctx := context.Background()
	_, _, key := newTestStore(t)
	journal := JournalKey{ID: "runtime-test-key", Key: make([]byte, 32)}
	journal.Key[0] = 7
	s, err := CreateRuntime(ctx, filepath.Join(t.TempDir(), "runtime.db"), "00000000-0000-4000-8000-000000000001", "key", key, journal, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.BootID() == "" {
		t.Fatal("missing boot identity")
	}
	if _, err = s.RecordReadyFixture(ctx, encodeFixture(t, prepareFixture(t))); !errors.Is(err, ErrSchema) {
		t.Fatalf("v5 fixture accepted: %v", err)
	}
}

func runtimeStore(t *testing.T) (*Store, string, []byte, JournalKey) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.db")
	key := bytes.Repeat([]byte{0x21}, 32)
	jk := JournalKey{ID: "runtime-key", Key: bytes.Repeat([]byte{0x31}, 32)}
	profile := RuntimeProfile{EndpointID: "00000000-0000-4000-8000-000000000009", PolicyRevision: 1, Fingerprint: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)), EngineID: "engine-one", DockerVersion: "27.1", ComposeVersion: "2.29"}
	s, err := CreateRuntime(ctx, path, testNode, "key", key, jk, RuntimeOptions{Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	hello := scanContractID(302)
	raw, err := protocol.NewMessage("welcome", rootScanSession, hello, map[string]any{"node_id": testNode, "session_id": rootScanSession, "reconcile_required": true})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindManagerSession(ctx, raw, hello); err != nil {
		t.Fatal(err)
	}
	rootEnable(t, s)
	return s, path, key, jk
}
func runtimePrepare(t *testing.T, s *Store) Record {
	t.Helper()
	m := prepareFixture(t)
	m["session_id"] = rootScanSession
	receipt, err := s.ReceivePrepare(context.Background(), encodeFixture(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return receipt.Record
}
func runtimeEvidence(t *testing.T, s *Store, r Record) PreparationEvidence {
	t.Helper()
	p, err := s.RuntimeProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return PreparationEvidence{OperationID: r.OperationID, InputFingerprint: r.LocalFingerprint, WorkspaceID: "workspaces/" + r.StackID + "/" + r.VersionID + "/package", ManifestFingerprint: p.Fingerprint, ProfileFingerprint: p.Fingerprint, EngineID: p.EngineID, DockerVersion: p.DockerVersion, ComposeVersion: p.ComposeVersion, Pre: Baseline{CollectedAt: timestamp(time.Now().UnixMilli()), Resources: []ResourceRecord{}}, Expected: []ExpectedResource{{Kind: "container", LogicalKey: "web/1"}}}
}
func TestRuntimePreparingIsAtomicAndInputCannotChange(t *testing.T) {
	s, path, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	if r.Phase != PhasePreparing {
		t.Fatalf("received directly ready: %s", r.Phase)
	}
	claims, err := s.Claims(ctx, r.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims %v %v", claims, err)
	}
	raw, err := s.ReadExecutionInput(ctx, r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	m := prepareFixture(t)
	body := m["body"].(map[string]any)
	want, _ := base64.StdEncoding.DecodeString(body["payload_b64"].(string))
	if !bytes.Equal(raw, want) {
		t.Fatal("input bytes changed")
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, raw) {
		t.Fatal("plaintext payload")
	}
	m["session_id"] = rootScanSession
	body["payload_b64"] = base64.StdEncoding.EncodeToString(append(want, ' '))
	if _, err = s.ReceivePrepare(ctx, encodeFixture(t, m)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed bytes: %v", err)
	}
	if _, err = s.CommitPrepared(ctx, r.OperationID, runtimeEvidence(t, s, r)); !errors.Is(err, ErrFrozen) {
		t.Fatalf("input conflict failed to freeze: %v", err)
	}
}

func runtimeStart(t *testing.T, s *Store, r Record, start func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CommitPrepared(ctx, r.OperationID, runtimeEvidence(t, s, r)); err != nil {
		t.Fatal(err)
	}
	q := startgate.Request{Session: rootScanSession, Operation: r.OperationID, Digest: r.Digest, Nonce: scanContractID(410), MessageID: scanContractID(411)}
	g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {
		if start != nil {
			start()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Authorize(startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce, PermitID: scanContractID(412), AuthRev: 1, ReplyTo: q.MessageID}); err != nil {
		t.Fatal(err)
	}
}
func runtimeObservation(t *testing.T, r Record, status string) ObservationEvidence {
	t.Helper()
	collected := timestamp(time.Now().UnixMilli())
	var errValue any
	if status != "complete" {
		errValue = map[string]any{"category": "unavailable", "code": "unavailable", "message": "query unavailable", "retry_action": "same_request"}
	}
	obs := map[string]any{"status": status, "collected_at": collected, "items": []any{}, "error": errValue}
	raw, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	return ObservationEvidence{Observation: raw, Post: Baseline{CollectedAt: collected, Resources: []ResourceRecord{}}}
}
func TestRuntimePreparationFailureAndRestartCancel(t *testing.T) {
	s, path, key, jk := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	ev, err := s.RecordPreparationFailure(ctx, r.OperationID, Failure{BootID: s.BootID(), Code: "preflight_failed"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 1 {
		t.Fatal(ev.Seq)
	}
	var body runtimeResult
	json.Unmarshal(ev.Body, &body)
	if body.Outcome != "not_started" || body.Error.Code != "preflight_failed" {
		t.Fatalf("%s", ev.Body)
	}
	identity := s.Identity()
	s.Close()
	reopened, err := OpenRuntime(ctx, path, identity, key, jk, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.RecordPreparationFailure(ctx, r.OperationID, Failure{BootID: reopened.BootID(), Code: "preflight_failed"}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("reopened preparation: %v", err)
	}
}
func TestRuntimeDisconnectKeepsProcessTruthAndObservationNewSequence(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	starts := 0
	runtimeStart(t, s, r, func() { starts++ })
	if err := s.RecordProcessCreated(ctx, r.OperationID, 1234); err != nil {
		t.Fatal(err)
	}
	if err := s.DisconnectManagerSession(ctx, rootScanSession); err != nil {
		t.Fatal(err)
	}
	zero := 0
	f := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli()), Output: []byte("same\noutput")}
	if err := s.RecordProcessExit(ctx, r.OperationID, f); err != nil {
		t.Fatal(err)
	}
	ev, err := s.RecordProcessOutcome(ctx, r.OperationID, f, runtimeObservation(t, r, "incomplete"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 2 {
		t.Fatal(ev.Seq)
	}
	query := scanContractID(413)
	if _, err = s.BeginObservationQuery(ctx, r.OperationID, query, 200); err != nil {
		t.Fatal(err)
	}
	newer, err := s.RecordObservationUpdate(ctx, r.OperationID, query, runtimeObservation(t, r, "complete"))
	if err != nil {
		t.Fatal(err)
	}
	if newer.Seq != 3 {
		t.Fatal(newer.Seq)
	}
	var a, b runtimeResult
	json.Unmarshal(ev.Body, &a)
	json.Unmarshal(newer.Body, &b)
	if a.Finished != b.Finished || *a.Exit != *b.Exit || *a.Output != *b.Output || b.Ownership != "consistent" || starts != 1 {
		t.Fatalf("changed process truth: %s %s starts=%d", ev.Body, newer.Body, starts)
	}
	replay, err := s.RecordObservationUpdate(ctx, r.OperationID, query, runtimeObservation(t, r, "error"))
	if err != nil || !bytes.Equal(replay.Body, newer.Body) {
		t.Fatalf("query replay: %s %v", replay.Body, err)
	}
	if _, err = s.RecoveryRecord(ctx, r.OperationID); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeOnlyLiveStartFailureCanProduceNotStartedSequenceTwo(t *testing.T) {
	s, path, key, jk := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	runtimeStart(t, s, r, nil)
	ev, err := s.RecordStartFailure(ctx, r.OperationID, Failure{BootID: s.BootID(), Code: "spawn_failed"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 2 {
		t.Fatal(ev.Seq)
	}
	s2, path2, key2, jk2 := runtimeStore(t)
	r2 := runtimePrepare(t, s2)
	runtimeStart(t, s2, r2, nil)
	identity := s2.Identity()
	s2.Close()
	reopened, err := OpenRuntime(ctx, path2, identity, key2, jk2, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.RecordStartFailure(ctx, r2.OperationID, Failure{BootID: reopened.BootID(), Code: "spawn_failed"}); err == nil {
		t.Fatal("reopened not_attempted became no-spawn evidence")
	}
	raw, err := reopened.RecoveryRecord(ctx, r2.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"phase":"reconciliation_required"`)) {
		t.Fatalf("%s", raw)
	}
	_ = path
	_ = key
	_ = jk
}

func runtimeResourceObservation(t *testing.T, r Record, id string) ObservationEvidence {
	t.Helper()
	e := runtimeObservation(t, r, "complete")
	resource := ResourceRecord{Kind: "container", ResourceRef: id, LogicalKey: "web/1", Classification: "managed", ProjectName: r.ProjectName, Service: "web", ContainerNumber: 1}
	e.Post.Resources = []ResourceRecord{resource}
	var obs map[string]any
	json.Unmarshal(e.Observation, &obs)
	obs["items"] = []any{map[string]any{"resource_kind": "container", "resource_id": id, "name": "sample-web-1", "project_name": r.ProjectName, "service_name": "web", "stack_id": r.StackID, "image_id": nil, "state": "running", "health": "none"}}
	e.Observation, _ = json.Marshal(obs)
	return e
}
func TestRuntimeFinalizeBaselineAndClaimsAreOneTransaction(t *testing.T) {
	s, path, key, jk := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	runtimeStart(t, s, r, nil)
	if err := s.RecordProcessCreated(ctx, r.OperationID, 987); err != nil {
		t.Fatal(err)
	}
	exit := 7
	fact := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &exit, FinishedAt: timestamp(time.Now().UnixMilli()), Output: []byte("native error")}
	observation := runtimeResourceObservation(t, r, strings.Repeat("a", 64))
	ev, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, observation)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, ev.Seq, rootScanSession); err != nil {
		t.Fatal(err)
	}
	binding, err := s.ReadProjectBinding(ctx, r.EndpointID, r.ProjectName)
	if err != nil || binding.Status != "provisional" || binding.Revision != 0 || len(binding.Baseline.Resources) != 0 {
		t.Fatalf("ACK published ownership: %+v %v", binding, err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER runtime_release_failure BEFORE DELETE ON local_claims BEGIN SELECT RAISE(ABORT,'release failed'); END`); err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(t, r, ev.Seq, "failed")
	if _, err = s.Finalize(ctx, request); !errors.Is(err, ErrStorage) {
		t.Fatalf("fault finalize: %v", err)
	}
	binding, err = s.ReadProjectBinding(ctx, r.EndpointID, r.ProjectName)
	claims, _ := s.Claims(ctx, r.OperationID)
	if err != nil || binding.Revision != 0 || binding.Status != "provisional" || len(claims) != 2 {
		t.Fatalf("half published: %+v claims=%v err=%v", binding, claims, err)
	}
	if _, err = s.ReadFinalized(ctx, r.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("receipt escaped rollback: %v", err)
	}
	s.db.Exec(`DROP TRIGGER runtime_release_failure`)
	reply, err := s.Finalize(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.validator.Validate(reply); err != nil {
		t.Fatal(err)
	}
	binding, err = s.ReadProjectBinding(ctx, r.EndpointID, r.ProjectName)
	claims, _ = s.Claims(ctx, r.OperationID)
	if err != nil || binding.Revision != 1 || binding.Status != "active" || len(binding.Baseline.Resources) != 1 || binding.Baseline.Resources[0].ResourceRef != strings.Repeat("a", 64) || len(claims) != 0 {
		t.Fatalf("publication: %+v %v %v", binding, claims, err)
	}
	if _, err = s.Finalize(ctx, request); err != nil {
		t.Fatalf("replay: %v", err)
	}
	identity := s.Identity()
	s.Close()
	reopened, err := OpenRuntime(ctx, path, identity, key, jk, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.ReadFinalized(ctx, r.OperationID); err != nil {
		t.Fatal(err)
	}
	binding, err = reopened.ReadProjectBinding(ctx, r.EndpointID, r.ProjectName)
	if err != nil || binding.Revision != 1 {
		t.Fatalf("reopen ownership: %+v %v", binding, err)
	}
}
func TestRuntimePrepareAndReadyFailuresLeaveNoHalfState(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	m := prepareFixture(t)
	m["session_id"] = rootScanSession
	s.db.Exec(`CREATE TRIGGER runtime_input_failure BEFORE INSERT ON runtime_documents WHEN NEW.kind='input' BEGIN SELECT RAISE(ABORT,'input failed'); END`)
	if _, err := s.ReceivePrepare(ctx, encodeFixture(t, m)); !errors.Is(err, ErrStorage) {
		t.Fatalf("failed receive: %v", err)
	}
	for _, table := range []string{"executions", "local_claims", "event_state", "runtime_documents"} {
		var n int
		s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n)
		if n != 0 {
			t.Fatalf("%s retained %d half rows", table, n)
		}
	}
	s.db.Exec(`DROP TRIGGER runtime_input_failure`)
	r := runtimePrepare(t, s)
	s.db.Exec(`CREATE TRIGGER runtime_ready_failure BEFORE UPDATE ON executions WHEN NEW.phase='ready' BEGIN SELECT RAISE(ABORT,'ready failed'); END`)
	if _, err := s.CommitPrepared(ctx, r.OperationID, runtimeEvidence(t, s, r)); !errors.Is(err, ErrStorage) {
		t.Fatalf("ready fault: %v", err)
	}
	stored, _ := s.Get(ctx, r.OperationID)
	var n int
	s.db.QueryRow(`SELECT count(*) FROM runtime_documents WHERE kind='preparation'`).Scan(&n)
	if stored.Phase != PhasePreparing || n != 0 {
		t.Fatalf("half ready: %+v evidence=%d", stored, n)
	}
	var uses int
	s.db.QueryRow(`SELECT uses FROM event_keys`).Scan(&uses)
	if uses < 3 {
		t.Fatal("failed encryption uses refunded")
	}
}
func runtimeBind(t *testing.T, s *Store) {
	t.Helper()
	hello := scanContractID(431)
	welcome, _ := protocol.NewMessage("welcome", rootScanSession, hello, map[string]any{"node_id": testNode, "session_id": rootScanSession, "reconcile_required": true})
	if err := s.BindManagerSession(context.Background(), welcome, hello); err != nil {
		t.Fatal(err)
	}
}
func runtimeCancel(t *testing.T, s *Store, r Record) CancelDecision {
	t.Helper()
	raw, _ := protocol.NewMessage("cancel", rootScanSession, "", map[string]any{"operation_id": r.OperationID, "input_digest": r.Digest})
	d, err := s.CancelExecution(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func TestRuntimeRestartCannotResumePreparingButCanCancel(t *testing.T) {
	s, path, key, jk := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	e := runtimeEvidence(t, s, r)
	identity := s.Identity()
	s.Close()
	reopened, err := OpenRuntime(ctx, path, identity, key, jk, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	runtimeBind(t, reopened)
	if _, err = reopened.CommitPrepared(ctx, r.OperationID, e); !errors.Is(err, ErrFrozen) {
		t.Fatalf("restarted prepare resumed: %v", err)
	}
	d := runtimeCancel(t, reopened, r)
	if d.Disposition != "not_started" || d.Event.Seq != 1 {
		t.Fatalf("restart cancel: %+v", d)
	}
	claims, _ := reopened.Claims(ctx, r.OperationID)
	if len(claims) != 2 {
		t.Fatalf("cancel released claims: %v", claims)
	}
}
func TestRuntimeRestartSavedExitCanReportButUnspawnedCommitStaysUnknown(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprint(saved), func(t *testing.T) {
			s, path, key, jk := runtimeStore(t)
			ctx := context.Background()
			r := runtimePrepare(t, s)
			runtimeStart(t, s, r, nil)
			zero := 0
			fact := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli()), Output: []byte("preserved")}
			if saved {
				if err := s.RecordProcessCreated(ctx, r.OperationID, 700); err != nil {
					t.Fatal(err)
				}
				if err := s.RecordProcessExit(ctx, r.OperationID, fact); err != nil {
					t.Fatal(err)
				}
			}
			identity := s.Identity()
			s.Close()
			reopened, err := OpenRuntime(ctx, path, identity, key, jk, RuntimeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			runtimeBind(t, reopened)
			if saved {
				ev, err := reopened.RecordProcessOutcome(ctx, r.OperationID, fact, runtimeObservation(t, r, "complete"))
				if err != nil || ev.Seq != 2 {
					t.Fatalf("saved exit lost: %s %v", ev.Body, err)
				}
			} else {
				d := runtimeCancel(t, reopened, r)
				if d.Disposition != "interrupt_required" || d.Event != nil {
					t.Fatalf("reopened not_attempted became not_started: %+v", d)
				}
			}
		})
	}
}
func TestRuntimeObservationAdmissionAndScanChanges(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	runtimeStart(t, s, r, nil)
	s.RecordProcessCreated(ctx, r.OperationID, 888)
	zero := 0
	fact := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli())}
	if _, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, runtimeObservation(t, r, "incomplete")); err != nil {
		t.Fatal(err)
	}
	query := scanContractID(440)
	first, err := s.BeginObservationQuery(ctx, r.OperationID, query, 20)
	if err != nil || first.Disposition != "collect" {
		t.Fatalf("first query %+v %v", first, err)
	}
	replay, err := s.BeginObservationQuery(ctx, r.OperationID, query, 20)
	if err != nil || replay.Disposition != "collecting" {
		t.Fatalf("duplicate collection %+v %v", replay, err)
	}
	if _, err = s.BeginObservationQuery(ctx, r.OperationID, query, 21); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed query accepted: %v", err)
	}
	if _, err = s.BeginObservationQuery(ctx, r.OperationID, scanContractID(441), 20); !errors.Is(err, ErrScanBusy) {
		t.Fatalf("parallel collect: %v", err)
	}
	page := rootPage(t, s, rootScanRequest(t, "pending", "", "", 200))
	newer, err := s.RecordObservationUpdate(ctx, r.OperationID, query, runtimeObservation(t, r, "complete"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteReconcile(ctx, rootCompleteRequest(t, s, page.ScanID)); !errors.Is(err, ErrScanChanged) {
		t.Fatalf("unacknowledged change completed: %v", err)
	}
	if err = s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, newer.Seq, rootScanSession); err != nil {
		t.Fatal(err)
	}
	rootComplete(t, s, page.ScanID)
	if _, err = s.BeginObservationQuery(ctx, r.OperationID, scanContractID(442), 20); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete result collected again: %v", err)
	}
	s.runtime.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if _, err = s.BeginObservationQuery(ctx, r.OperationID, query, 20); !errors.Is(err, ErrScanExpired) {
		t.Fatalf("expired replay collected: %v", err)
	}
}

func TestRuntimeFormatKeysProfileAndFixturesRemainSeparated(t *testing.T) {
	s, path, key, jk := runtimeStore(t)
	ctx := context.Background()
	identity := s.Identity()
	if _, err := OpenRelease(ctx, path, identity, key, jk, RecoveryOptions{}); !errors.Is(err, ErrSchema) {
		t.Fatalf("v4 opened v5: %v", err)
	}
	if _, err := OpenRuntime(ctx, path, identity, bytes.Repeat([]byte{0xff}, 32), jk, RuntimeOptions{}); !errors.Is(err, ErrKey) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := OpenRuntime(ctx, path, identity, key, JournalKey{ID: jk.ID, Key: bytes.Repeat([]byte{0xff}, 32)}, RuntimeOptions{}); !errors.Is(err, ErrKey) {
		t.Fatalf("wrong journal key: %v", err)
	}
	if err := s.BindRecoverySessionFixture(ctx, rootScanSession); !errors.Is(err, ErrSchema) {
		t.Fatalf("fixture binding: %v", err)
	}
	if _, err := s.RecordResultFixture(ctx, []byte(`{}`)); !errors.Is(err, ErrSchema) {
		t.Fatalf("fixture outcome: %v", err)
	}
	legacy, legacyPath, legacyKey, legacyJournal := releaseStore(t)
	if _, err := OpenRuntime(ctx, legacyPath, legacy.Identity(), legacyKey, legacyJournal, RuntimeOptions{}); !errors.Is(err, ErrSchema) {
		t.Fatalf("v5 migrated v4: %v", err)
	}
	profile, _ := s.RuntimeProfile(ctx)
	other := profile
	other.EngineID = "different-engine"
	if err := s.BindRuntimeProfile(ctx, other); !errors.Is(err, ErrIdentity) {
		t.Fatalf("engine replaced: %v", err)
	}
	r := runtimePrepare(t, s)
	other = profile
	other.PolicyRevision++
	other.ComposeVersion = "new-compose"
	if err := s.BindRuntimeProfile(ctx, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("profile changed with claims: %v", err)
	}
	evidence := runtimeEvidence(t, s, r)
	evidence.EngineID = "different-engine"
	if _, err := s.CommitPrepared(ctx, r.OperationID, evidence); !errors.Is(err, ErrConflict) {
		t.Fatalf("unverified evidence: %v", err)
	}
}
func TestRuntimeFirstProjectCollisionCannotBeAdopted(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	e := runtimeEvidence(t, s, r)
	e.Pre.Resources = []ResourceRecord{{Kind: "container", ResourceRef: strings.Repeat("b", 64), LogicalKey: "web/1", Classification: "managed", ProjectName: r.ProjectName, Service: "web", ContainerNumber: 1}}
	if _, err := s.CommitPrepared(ctx, r.OperationID, e); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign project adopted: %v", err)
	}
	e.Pre.Resources[0].Classification = "external"
	if _, err := s.CommitPrepared(ctx, r.OperationID, e); !errors.Is(err, ErrEvidence) {
		t.Fatalf("foreign container relabeled external: %v", err)
	}
	if _, err := s.ReadProjectBinding(ctx, r.EndpointID, r.ProjectName); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed precheck created binding: %v", err)
	}
}
func TestRuntimeInterruptedExitZeroNeverBecomesNormalOrReleased(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	runtimeStart(t, s, r, nil)
	if err := s.RecordProcessCreated(ctx, r.OperationID, 54); err != nil {
		t.Fatal(err)
	}
	d := runtimeCancel(t, s, r)
	if d.Disposition != "interrupt_required" {
		t.Fatal(d)
	}
	zero := 0
	f := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli())}
	if err := s.RecordProcessExit(ctx, r.OperationID, f); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel exit0 became normal: %v", err)
	}
	f.Outcome = "interrupted"
	f.InterruptReason = "canceled"
	ev, err := s.RecordProcessOutcome(ctx, r.OperationID, f, runtimeObservation(t, r, "complete"))
	if err != nil {
		t.Fatal(err)
	}
	var result runtimeResult
	json.Unmarshal(ev.Body, &result)
	if result.Ownership != "unknown" || result.Outcome != "interrupted" || result.Exit == nil || *result.Exit != 0 {
		t.Fatalf("%s", ev.Body)
	}
	if _, err = s.BeginObservationQuery(ctx, r.OperationID, scanContractID(454), 200); !errors.Is(err, ErrConflict) {
		t.Fatalf("interrupted became observe eligible: %v", err)
	}
	if _, err = s.Finalize(ctx, releaseRequest(t, r, ev.Seq, "succeeded")); !errors.Is(err, ErrConflict) {
		t.Fatalf("interrupted finalized: %v", err)
	}
	claims, _ := s.Claims(ctx, r.OperationID)
	if len(claims) != 2 {
		t.Fatalf("interrupted claims %v", claims)
	}
}
func TestRuntimeReadyReplayDoesNotSpendMoreEncryptionBudget(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	e := runtimeEvidence(t, s, r)
	if _, err := s.CommitPrepared(ctx, r.OperationID, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE event_keys SET uses=?`, maxKeyUses); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitPrepared(ctx, r.OperationID, e); err != nil {
		t.Fatalf("ready replay reserved key quota: %v", err)
	}
	m := prepareFixture(t)
	m["session_id"] = rootScanSession
	receipt, err := s.ReceivePrepare(ctx, encodeFixture(t, m))
	if err != nil || receipt.Disposition != "ready" {
		t.Fatalf("prepare replay reserved quota: %+v %v", receipt, err)
	}
}

func TestRuntimeOwnershipReplacementAndExternalDrift(t *testing.T) {
	for _, legitimate := range []bool{true, false} {
		t.Run(fmt.Sprint(legitimate), func(t *testing.T) {
			s, _, _, _ := runtimeStore(t)
			ctx := context.Background()
			r := runtimePrepare(t, s)
			runtimeStart(t, s, r, nil)
			s.RecordProcessCreated(ctx, r.OperationID, 21)
			zero := 0
			fact := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli())}
			oldID, newID := strings.Repeat("a", 64), strings.Repeat("b", 64)
			initial, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, runtimeResourceObservation(t, r, oldID))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Finalize(ctx, releaseRequest(t, r, initial.Seq, "succeeded")); err != nil {
				t.Fatal(err)
			}
			binding, err := s.ReadProjectBinding(ctx, r.EndpointID, r.ProjectName)
			if err != nil {
				t.Fatal(err)
			}
			m := prepareFixture(t)
			m["session_id"] = rootScanSession
			m["body"].(map[string]any)["operation_id"] = scanContractID(480)
			receipt, err := s.ReceivePrepare(ctx, encodeFixture(t, m))
			if err != nil {
				t.Fatal(err)
			}
			r2 := receipt.Record
			prep := runtimeEvidence(t, s, r2)
			prep.OwnershipRevision = binding.Revision
			prep.Pre = binding.Baseline
			prep.Pre.CollectedAt = timestamp(time.Now().UnixMilli())
			if _, err = s.CommitPrepared(ctx, r2.OperationID, prep); err != nil {
				t.Fatal(err)
			}
			q := startgate.Request{Session: rootScanSession, Operation: r2.OperationID, Digest: r2.Digest, Nonce: scanContractID(481), MessageID: scanContractID(482)}
			g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {})
			if err != nil {
				t.Fatal(err)
			}
			if err = g.Authorize(startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce, PermitID: scanContractID(483), ReplyTo: q.MessageID, AuthRev: 2}); err != nil {
				t.Fatal(err)
			}
			if err = s.RecordProcessCreated(ctx, r2.OperationID, 22); err != nil {
				t.Fatal(err)
			}
			post := runtimeResourceObservation(t, r2, newID)
			if legitimate {
				post.Replacements = []Replacement{{Kind: "container", LogicalKey: "web/1", OldRef: oldID, NewRef: newID}}
			}
			fact.FinishedAt = timestamp(time.Now().UnixMilli())
			event, err := s.RecordProcessOutcome(ctx, r2.OperationID, fact, post)
			if err != nil {
				t.Fatal(err)
			}
			var result runtimeResult
			json.Unmarshal(event.Body, &result)
			if legitimate {
				if result.Ownership != "consistent" {
					t.Fatalf("authorized replacement not explained: %s", event.Body)
				}
				unchanged, _ := s.ReadProjectBinding(ctx, r2.EndpointID, r2.ProjectName)
				if unchanged.Revision != 1 || unchanged.Baseline.Resources[0].ResourceRef != oldID {
					t.Fatal("result published baseline")
				}
				if _, err = s.Finalize(ctx, releaseRequest(t, r2, event.Seq, "succeeded")); err != nil {
					t.Fatal(err)
				}
				updated, _ := s.ReadProjectBinding(ctx, r2.EndpointID, r2.ProjectName)
				if updated.Revision != 2 || updated.Baseline.Resources[0].ResourceRef != newID {
					t.Fatalf("replacement publication: %+v", updated)
				}
				fact.Output = []byte("late contradiction")
				if err = s.RecordProcessExit(ctx, r2.OperationID, fact); !errors.Is(err, ErrConflict) {
					t.Fatalf("late contradiction: %v", err)
				}
				var quarantined bool
				s.db.QueryRow(`SELECT quarantined FROM release_control`).Scan(&quarantined)
				if !quarantined {
					t.Fatal("released contradiction did not quarantine")
				}
			} else {
				if result.Ownership != "drifted" {
					t.Fatalf("unexplained external replacement adopted: %s", event.Body)
				}
				unchanged, _ := s.ReadProjectBinding(ctx, r2.EndpointID, r2.ProjectName)
				if unchanged.Revision != 1 || unchanged.Status != "drifted" || unchanged.Baseline.Resources[0].ResourceRef != oldID {
					t.Fatalf("drift publication: %+v", unchanged)
				}
				if _, err = s.BeginObservationQuery(ctx, r2.OperationID, scanContractID(484), 200); !errors.Is(err, ErrConflict) {
					t.Fatalf("drift refresh adopted: %v", err)
				}
			}
		})
	}
}
func TestRuntimeUnknownOutcomeCanBeStoredWithoutObservation(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	runtimeStart(t, s, r, nil)
	s.RecordProcessCreated(ctx, r.OperationID, 55)
	fact := ProcessOutcome{BootID: s.BootID(), Outcome: "unknown", FinishedAt: timestamp(time.Now().UnixMilli()), ErrorCode: "wait_unavailable"}
	event, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, ObservationEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	var result runtimeResult
	json.Unmarshal(event.Body, &result)
	if result.Outcome != "unknown" || result.Ownership != "unknown" || !bytes.Equal(result.Observation, []byte("null")) {
		t.Fatalf("unknown process invented observation: %s", event.Body)
	}
}

func TestRuntimeExpiredAfterCommitUsesLiveNoSpawnEvidence(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	if _, err := s.CommitPrepared(ctx, r.OperationID, runtimeEvidence(t, s, r)); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	calls, starts := 0, 0
	now := func() time.Time {
		calls++
		if calls <= 2 {
			return base
		}
		return base.Add(11 * time.Second)
	}
	q := startgate.Request{Session: rootScanSession, Operation: r.OperationID, Digest: r.Digest, Nonce: scanContractID(490), MessageID: scanContractID(491)}
	g, err := s.NewGate(ctx, q, now, func(startgate.Permit) { starts++ })
	if err != nil {
		t.Fatal(err)
	}
	err = g.Authorize(startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce, PermitID: scanContractID(492), ReplyTo: q.MessageID, AuthRev: 1})
	if !errors.Is(err, startgate.ErrExpired) || starts != 0 {
		t.Fatalf("expired permit spawned: %v starts=%d", err, starts)
	}
	event, err := s.RecordStartFailure(ctx, r.OperationID, Failure{BootID: s.BootID(), Code: "permit_expired"})
	if err != nil || event.Seq != 2 {
		t.Fatalf("live expiry proof: seq%d %v", event.Seq, err)
	}
	if _, err = s.RecoveryRecord(ctx, r.OperationID); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeUncertainCommitCannotCreateNoSpawnResult(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	s.CommitPrepared(ctx, r.OperationID, runtimeEvidence(t, s, r))
	s.db.Exec(`CREATE TRIGGER runtime_commit_failure BEFORE INSERT ON process_facts BEGIN SELECT RAISE(ABORT,'commit failed'); END`)
	q := startgate.Request{Session: rootScanSession, Operation: r.OperationID, Digest: r.Digest, Nonce: scanContractID(495), MessageID: scanContractID(496)}
	starts := 0
	g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) { starts++ })
	if err != nil {
		t.Fatal(err)
	}
	err = g.Authorize(startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce, PermitID: scanContractID(497), ReplyTo: q.MessageID, AuthRev: 1})
	if !errors.Is(err, startgate.ErrCommitFailed) || starts != 0 {
		t.Fatalf("failed commit spawned: %v starts=%d", err, starts)
	}
	if _, err = s.RecordStartFailure(ctx, r.OperationID, Failure{BootID: s.BootID(), Code: "spawn_failed"}); err == nil {
		t.Fatal("uncertain commit became no-spawn result")
	}
	raw, _ := protocol.NewMessage("cancel", rootScanSession, "", map[string]any{"operation_id": r.OperationID, "input_digest": r.Digest})
	decision, err := s.CancelExecution(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Event != nil {
		t.Fatalf("uncertain commit canceled with no-spawn evidence: %+v", decision)
	}
}

func TestRuntimeSchemaKeepsRecoveryPageLimit(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	if _, err := s.db.Exec(`UPDATE recovery_control SET frozen=1,active_scan_id=?,active_scope='pending',active_limit=201 WHERE id=1`, scanContractID(499)); err == nil {
		t.Fatal("v5 relaxed recovery page limit above 200")
	}
}
