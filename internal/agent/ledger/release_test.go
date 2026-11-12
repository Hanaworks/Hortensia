package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

func releaseStore(t *testing.T) (*Store, string, []byte, JournalKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.db")
	key := bytes.Repeat([]byte{0x55}, 32)
	jk := JournalKey{ID: "release-kek", Key: bytes.Repeat([]byte{0x61}, 32)}
	s, err := CreateRelease(context.Background(), path, testNode, "release-key", key, jk, RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.BindRecoverySessionFixture(context.Background(), rootScanSession); err != nil {
		t.Fatal(err)
	}
	rootEnable(t, s)
	return s, path, key, jk
}

func releaseRequest(t *testing.T, r Record, seq uint64, terminal string) []byte {
	t.Helper()
	raw, err := protocol.NewMessage("finalize", rootScanSession, "", map[string]any{"operation_id": r.OperationID, "input_digest": r.Digest, "result_event_seq": seq, "terminal_state": terminal})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReleaseAtomicReplayAndReopen(t *testing.T) {
	s, path, key, jk := releaseStore(t)
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	rawResult := resultFixtureFor(t, r, "not_started", 1)
	e, err := s.RecordResultFixture(ctx, rawResult)
	if err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(t, r, 1, "failed")
	response, err := s.Finalize(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.validator.Validate(response)
	if err != nil || env.Type != "finalized" {
		t.Fatalf("response: %v", err)
	}
	req, _ := s.validator.Validate(request)
	if env.ReplyTo == nil || *env.ReplyTo != req.MessageID {
		t.Fatal("wrong reply correlation")
	}
	claims, err := s.Claims(ctx, r.OperationID)
	if err != nil || len(claims) != 0 {
		t.Fatalf("claims: %v %v", claims, err)
	}
	stored, err := s.ReadFinalized(ctx, r.OperationID)
	if err != nil || !bytes.Equal(stored, env.Body) {
		t.Fatalf("receipt: %v", err)
	}
	var uses int
	s.db.QueryRow("SELECT uses FROM event_keys").Scan(&uses)
	s.db.Exec("UPDATE event_keys SET uses=?", maxKeyUses)
	if _, err = s.Finalize(ctx, request); err != nil {
		t.Fatalf("exhausted replay: %v", err)
	}
	if replay, err := s.RecordResultFixture(ctx, rawResult); err != nil || !bytes.Equal(replay.Body, e.Body) {
		t.Fatalf("released event replay: %v", err)
	}
	var after int64
	s.db.QueryRow("SELECT uses FROM event_keys").Scan(&after)
	if after != maxKeyUses || uses != 2 {
		t.Fatalf("key budget %d %d", uses, after)
	}
	view, err := s.RecoveryRecord(ctx, r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	json.Unmarshal(view, &v)
	if v["phase"] != "finalized" || v["claims_held"] != false || v["final_state"] != "failed" {
		t.Fatalf("view %s", view)
	}
	id := s.Identity()
	s.Close()
	reopened, err := OpenRelease(ctx, path, id, key, jk, RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored2, err := reopened.ReadFinalized(ctx, r.OperationID)
	if err != nil || !bytes.Equal(stored, stored2) {
		t.Fatalf("reopen evidence: %v", err)
	}
	state, err := reopened.RecoveryScanState(ctx)
	if err != nil || !state.Frozen {
		t.Fatal("reopen thawed")
	}
}

func TestReleaseWrongFactPersistsQuarantine(t *testing.T) {
	s, path, key, jk := releaseStore(t)
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	result := resultFixtureFor(t, r, "not_started", 1)
	if _, err := s.RecordResultFixture(ctx, result); err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(t, r, 1, "failed")
	if _, err := s.Finalize(ctx, request); err != nil {
		t.Fatal(err)
	}
	original, err := s.ReadFinalized(ctx, r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.Finalize(ctx, releaseRequest(t, r, 1, "succeeded")); !errors.Is(err, ErrConflict) || len(out) != 0 {
		t.Fatalf("contradiction %v", err)
	}
	var quarantined bool
	if err := s.db.QueryRow("SELECT quarantined FROM release_control WHERE id=1").Scan(&quarantined); err != nil || !quarantined {
		t.Fatalf("quarantine %v", err)
	}
	if out, err := s.Finalize(ctx, request); err == nil || len(out) != 0 {
		t.Fatal("quarantine replay succeeded")
	}
	claims, _ := s.Claims(ctx, r.OperationID)
	if len(claims) != 0 {
		t.Fatal("invented claims")
	}
	id := s.Identity()
	s.Close()
	s, err = OpenRelease(ctx, path, id, key, jk, RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindRecoverySessionFixture(ctx, rootScanSession); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Reconcile(ctx, rootScanRequest(t, "all", "", "", 200)); err == nil || len(out) != 0 {
		t.Fatal("new scan erased quarantine")
	}
	if now, err := s.ReadFinalized(ctx, r.OperationID); err != nil || !bytes.Equal(now, original) {
		t.Fatalf("diagnostic receipt lost: %v", err)
	}
}

func TestReleaseTerminalMatrix(t *testing.T) {
	cases := []struct {
		name, outcome, terminal string
		change                  func(map[string]any)
		ok                      bool
	}{
		{"not started", "not_started", "failed", nil, true},
		{"canceled", "not_started", "canceled", func(b map[string]any) { b["error"].(map[string]any)["code"] = "canceled_before_start" }, true},
		{"success", "normal_exit", "succeeded", nil, true},
		{"nonzero", "normal_exit", "failed", func(b map[string]any) {
			b["exit_code"] = 3
			b["error"] = map[string]any{"category": "execution_failed", "code": "exit_nonzero", "message": "fixture", "retry_action": "query_operation"}
		}, true},
		{"unknown", "unknown", "failed", nil, false},
		{"interrupted", "interrupted", "canceled", nil, false},
		{"partial", "normal_exit", "succeeded", func(b map[string]any) {
			b["observation"].(map[string]any)["status"] = "incomplete"
			b["observation"].(map[string]any)["error"] = map[string]any{"category": "execution_failed", "code": "incomplete", "message": "fixture", "retry_action": "query_operation"}
		}, false},
		{"ownership", "normal_exit", "succeeded", func(b map[string]any) { b["ownership_status"] = "unknown" }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, _, _ := releaseStore(t)
			ctx := context.Background()
			r, _, p := gateReady(t, s)
			seq := uint64(1)
			if c.outcome != "not_started" {
				if err := s.commitStart(ctx, p); err != nil {
					t.Fatal(err)
				}
				seq = 2
			}
			raw := resultFixtureFor(t, r, c.outcome, seq)
			if c.change != nil {
				var m map[string]any
				json.Unmarshal(raw, &m)
				c.change(m["body"].(map[string]any))
				raw = encodeFixture(t, m)
			}
			if _, err := s.RecordResultFixture(ctx, raw); err != nil {
				t.Fatal(err)
			}
			out, err := s.Finalize(ctx, releaseRequest(t, r, seq, c.terminal))
			if c.ok {
				if err != nil {
					t.Fatal(err)
				}
				env, err := s.validator.Validate(out)
				if err != nil {
					t.Fatal(err)
				}
				p, _ := parseFinalize(env.Body)
				if p.Terminal != c.terminal {
					t.Fatal("terminal mismatch")
				}
			} else if err == nil || len(out) != 0 {
				t.Fatal("uncertain result released")
			}
			claims, _ := s.Claims(ctx, r.OperationID)
			want := 2
			if c.ok {
				want = 0
			}
			if len(claims) != want {
				t.Fatalf("claims=%d want=%d", len(claims), want)
			}
		})
	}
}

func TestReleasePendingAllAckAndSnapshot(t *testing.T) {
	s, _, _, _ := releaseStore(t)
	ctx := context.Background()
	r, _, p := gateReady(t, s)
	if err := s.commitStart(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "normal_exit", 2)); err != nil {
		t.Fatal(err)
	}
	old := rootPage(t, s, rootScanRequest(t, "pending", "", "", 200))
	if _, err := s.Finalize(ctx, releaseRequest(t, r, 2, "succeeded")); err != nil {
		t.Fatal(err)
	}
	again := rootPage(t, s, rootScanRequest(t, "pending", old.ScanID, "", 200))
	if !bytes.Equal(old.Records[0], again.Records[0]) {
		t.Fatal("finalize rewrote fixed snapshot")
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 2, rootScanSession); err != nil {
		t.Fatal(err)
	}
	rootComplete(t, s, old.ScanID)
	pending := rootPage(t, s, rootScanRequest(t, "pending", "", "", 200))
	if pending.Total != 1 {
		t.Fatal("omitted unacked started event")
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, rootScanSession); err != nil {
		t.Fatal(err)
	}
	rootComplete(t, s, pending.ScanID)
	pending = rootPage(t, s, rootScanRequest(t, "pending", "", "", 200))
	if pending.Total != 0 {
		t.Fatal("acknowledged history still pending")
	}
	all := rootPage(t, s, rootScanRequest(t, "all", "", "", 200))
	if all.Total != 1 {
		t.Fatal("all lost finalized history")
	}
	var v map[string]any
	json.Unmarshal(all.Records[0], &v)
	if v["phase"] != "finalized" || v["claims_held"] != false {
		t.Fatal("bad finalized projection")
	}
}

func TestReleasePendingAuthenticatesOmittedHistory(t *testing.T) {
	for _, table := range []string{"finalizations", "execution_events"} {
		t.Run(table, func(t *testing.T) {
			s, _, _, _ := releaseStore(t)
			ctx := context.Background()
			r, _, _ := gateReady(t, s)
			if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Finalize(ctx, releaseRequest(t, r, 1, "failed")); err != nil {
				t.Fatal(err)
			}
			if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, rootScanSession); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE " + table + " SET body_cipher=zeroblob(length(body_cipher))"); err != nil {
				t.Fatal(err)
			}
			if out, err := s.Reconcile(ctx, rootScanRequest(t, "pending", "", "", 200)); !errors.Is(err, ErrEvidence) || len(out) != 0 {
				t.Fatalf("corrupt history hidden by filter: %v", err)
			}
			if _, err := s.ReadFinalized(ctx, r.OperationID); !errors.Is(err, ErrEvidence) {
				t.Fatalf("corrupt evidence accepted: %v", err)
			}
		})
	}
}

func TestReleaseFaultAtomicity(t *testing.T) {
	for _, fault := range []string{"insert", "delete", "commit"} {
		t.Run(fault, func(t *testing.T) {
			s, _, _, _ := releaseStore(t)
			ctx := context.Background()
			r, _, _ := gateReady(t, s)
			event, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1))
			if err != nil {
				t.Fatal(err)
			}
			sql := `CREATE TRIGGER fail_release BEFORE INSERT ON finalizations BEGIN SELECT RAISE(ABORT,'injected'); END`
			if fault == "delete" {
				sql = `CREATE TRIGGER fail_release BEFORE DELETE ON local_claims BEGIN SELECT RAISE(ABORT,'injected'); END`
			}
			if fault == "commit" {
				sql = `CREATE TABLE fault_release(parent TEXT REFERENCES executions(operation_id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_release AFTER DELETE ON local_claims BEGIN INSERT INTO fault_release(parent) VALUES('missing'); END`
			}
			if _, err = s.db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			if out, err := s.Finalize(ctx, releaseRequest(t, r, 1, "failed")); !errors.Is(err, ErrStorage) || len(out) != 0 {
				t.Fatalf("fault %v", err)
			}
			claims, err := s.Claims(ctx, r.OperationID)
			if err != nil || len(claims) != 2 {
				t.Fatal("partial claim release")
			}
			var receipts int
			s.db.QueryRow("SELECT count(*) FROM finalizations").Scan(&receipts)
			if receipts != 0 {
				t.Fatal("partial receipt")
			}
			original, err := s.ReadEvent(ctx, r.OperationID, 1)
			if err != nil || !bytes.Equal(event.Body, original.Body) {
				t.Fatal("fault damaged original")
			}
			var uses int
			s.db.QueryRow("SELECT uses FROM event_keys").Scan(&uses)
			if uses != 2 {
				t.Fatal("failed seal reservation refunded")
			}
			if _, err = s.db.Exec("DROP TRIGGER fail_release"); err != nil {
				t.Fatal(err)
			}
			if fault == "commit" {
				if _, err = s.db.Exec("DROP TABLE fault_release"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.Finalize(ctx, releaseRequest(t, r, 1, "failed")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReleaseEventConflictAndFailedQuarantineBarrier(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "write failure"}[fail], func(t *testing.T) {
			s, _, _, _ := releaseStore(t)
			ctx := context.Background()
			r, _, _ := gateReady(t, s)
			raw := resultFixtureFor(t, r, "not_started", 1)
			event, err := s.RecordResultFixture(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			request := releaseRequest(t, r, 1, "failed")
			if _, err = s.Finalize(ctx, request); err != nil {
				t.Fatal(err)
			}
			receipt, err := s.ReadFinalized(ctx, r.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err = s.db.Exec(`CREATE TRIGGER fail_quarantine BEFORE UPDATE ON release_control BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
			}
			changed := bytes.Replace(raw, []byte("2026-11-04T04:00:00.000Z"), []byte("2026-11-04T04:00:01.000Z"), 1)
			if _, err = s.RecordResultFixture(ctx, changed); err == nil {
				t.Fatal("conflict accepted")
			}
			if fail {
				if _, err = s.db.Exec("DROP TRIGGER fail_quarantine"); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := s.Finalize(ctx, request); err == nil || len(out) != 0 {
				t.Fatal("lost local barrier")
			}
			if out, err := s.Reconcile(ctx, rootScanRequest(t, "all", "", "", 200)); err == nil || len(out) != 0 {
				t.Fatal("scan reset local barrier")
			}
			now, err := s.ReadFinalized(ctx, r.OperationID)
			if err != nil || !bytes.Equal(now, receipt) {
				t.Fatal("receipt changed")
			}
			original, err := s.ReadEvent(ctx, r.OperationID, 1)
			if err != nil || !bytes.Equal(original.Body, event.Body) {
				t.Fatal("original changed")
			}
			claims, _ := s.Claims(ctx, r.OperationID)
			if len(claims) != 0 {
				t.Fatal("invented claims")
			}
		})
	}
}

func TestReleaseVersionsAndStaleSession(t *testing.T) {
	s, path, key, jk := releaseStore(t)
	ctx := context.Background()
	id := s.Identity()
	var version int
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 4 {
		t.Fatal(version)
	}
	for _, open := range []func() (*Store, error){func() (*Store, error) { return Open(ctx, path, id, key) }, func() (*Store, error) { return OpenJournal(ctx, path, id, key, jk) }, func() (*Store, error) { return OpenRecovery(ctx, path, id, key, jk, RecoveryOptions{}) }} {
		if old, err := open(); !errors.Is(err, ErrSchema) {
			if old != nil {
				old.Close()
			}
			t.Fatalf("old format accepted: %v", err)
		}
	}
	old, p, k, j := rootScanStore(t, nil)
	if opened, err := OpenRelease(ctx, p, old.Identity(), k, j, RecoveryOptions{}); !errors.Is(err, ErrSchema) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("v4 accepted v3: %v", err)
	}
	if _, err := old.Finalize(ctx, []byte("{}")); !errors.Is(err, ErrRecovery) {
		t.Fatalf("old API: %v", err)
	}
	r, _, _ := gateReady(t, s)
	if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); err != nil {
		t.Fatal(err)
	}
	request := releaseRequest(t, r, 1, "failed")
	var m map[string]any
	json.Unmarshal(request, &m)
	m["session_id"] = gateOtherID
	if _, err := s.Finalize(ctx, encodeFixture(t, m)); !errors.Is(err, ErrFrozen) {
		t.Fatalf("stale session: %v", err)
	}
	var q bool
	s.db.QueryRow("SELECT quarantined FROM release_control").Scan(&q)
	if q {
		t.Fatal("stale session polluted current facts")
	}
	if _, err := s.Finalize(ctx, request); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseWrongFactsPreserveEvidence(t *testing.T) {
	for _, wrong := range []string{"digest", "sequence", "terminal", "missing result"} {
		t.Run(wrong, func(t *testing.T) {
			s, _, _, _ := releaseStore(t)
			ctx := context.Background()
			r, _, _ := gateReady(t, s)
			if wrong != "missing result" {
				if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); err != nil {
					t.Fatal(err)
				}
			}
			req := releaseRequest(t, r, 1, "failed")
			var m map[string]any
			json.Unmarshal(req, &m)
			b := m["body"].(map[string]any)
			switch wrong {
			case "digest":
				b["input_digest"] = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
			case "sequence":
				b["result_event_seq"] = 2
			case "terminal":
				b["terminal_state"] = "canceled"
			}
			if out, err := s.Finalize(ctx, encodeFixture(t, m)); !errors.Is(err, ErrConflict) || len(out) != 0 {
				t.Fatalf("wrong fact accepted: %v", err)
			}
			claims, _ := s.Claims(ctx, r.OperationID)
			if len(claims) != 2 {
				t.Fatal("wrong fact released claims")
			}
			var q bool
			if err := s.db.QueryRow("SELECT quarantined FROM release_control").Scan(&q); err != nil || !q {
				t.Fatalf("quarantine missing: %v", err)
			}
			var count int
			s.db.QueryRow("SELECT count(*) FROM finalizations").Scan(&count)
			if count != 0 {
				t.Fatal("wrong fact produced evidence")
			}
		})
	}
}

func TestReleaseOpenRejectsWrongIdentityKeyAndDDL(t *testing.T) {
	s, path, key, jk := releaseStore(t)
	ctx := context.Background()
	id := s.Identity()
	other := id
	other.LedgerID = gateOtherID
	cases := []struct {
		id   Identity
		key  []byte
		jk   JournalKey
		want error
	}{
		{other, key, jk, ErrIdentity}, {id, bytes.Repeat([]byte{3}, 32), jk, ErrKey},
		{id, key, JournalKey{ID: jk.ID, Key: bytes.Repeat([]byte{2}, 32)}, ErrKey},
	}
	for _, c := range cases {
		opened, err := OpenRelease(ctx, path, c.id, c.key, c.jk, RecoveryOptions{})
		if opened != nil {
			opened.Close()
		}
		if !errors.Is(err, c.want) {
			t.Fatalf("open got %v want %v", err, c.want)
		}
	}
	if _, err := s.db.Exec("CREATE TABLE foreign_object(x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenRelease(ctx, path, id, key, jk, RecoveryOptions{})
	if opened != nil {
		opened.Close()
	}
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("foreign DDL accepted %v", err)
	}
}

func TestReleaseResultDoesNotAcquireScanMutex(t *testing.T) {
	s, _, _, _ := releaseStore(t)
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	raw := resultFixtureFor(t, r, "not_started", 1)
	s.recovery.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := s.RecordResultFixture(ctx, raw); done <- err }()
	blocked := false
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(300 * time.Millisecond):
		blocked = true
	}
	s.recovery.mu.Unlock()
	if blocked {
		if err := <-done; err != nil {
			t.Error(err)
		}
		t.Fatal("result callback waits for scan mutex")
	}
}

func TestReleaseConflictInsertFailureStillBlocks(t *testing.T) {
	s, _, _, _ := releaseStore(t)
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	raw := resultFixtureFor(t, r, "not_started", 1)
	if _, err := s.RecordResultFixture(ctx, raw); err != nil {
		t.Fatal(err)
	}
	req := releaseRequest(t, r, 1, "failed")
	if _, err := s.Finalize(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_conflict BEFORE INSERT ON event_conflicts BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(raw, []byte("2026-11-04T04:00:00.000Z"), []byte("2026-11-04T04:00:01.000Z"), 1)
	if _, err := s.RecordResultFixture(ctx, changed); !errors.Is(err, ErrStorage) {
		t.Fatalf("conflict insert fault %v", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_conflict"); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Finalize(ctx, req); err == nil || len(out) != 0 {
		t.Fatal("conflict insert failure lost local barrier")
	}
}

func TestReleaseStartCallbackReportsResultDuringScan(t *testing.T) {
	s, _, _, _ := releaseStore(t)
	ctx := context.Background()
	r, q, p := gateReady(t, s)
	entered := make(chan struct{})
	report := make(chan struct{})
	reported := make(chan error, 1)
	raw := resultFixtureFor(t, r, "normal_exit", 2)
	gate, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {
		close(entered)
		<-report
		_, err := s.RecordResultFixture(ctx, raw)
		reported <- err
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- gate.Authorize(p) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("start callback not entered")
	}
	scanning := make(chan error, 1)
	request := rootScanRequest(t, "pending", "", "", 200)
	var pageRaw []byte
	go func() { out, err := s.Reconcile(ctx, request); pageRaw = out; scanning <- err }()
	deadline := time.Now().Add(2 * time.Second)
	frozen := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		frozen = s.recovery.localFrozen
		s.mu.Unlock()
		if frozen {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(report)
	if !frozen {
		t.Fatal("scan did not freeze before waiting on callback")
	}
	for _, done := range []chan error{reported, started, scanning} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("callback and scanner deadlocked")
		}
	}
	event, err := s.ReadEvent(ctx, r.OperationID, 2)
	if err != nil || event.Kind != "result" {
		t.Fatalf("result not persisted: %v", err)
	}
	if err = s.commitStart(ctx, p); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen ledger admitted a start: %v", err)
	}
	// Gate invalidation waits before the scan freezes durably; the snapshot must
	// include the result even if it precedes the durable change index boundary.
	env, err := s.validator.Validate(pageRaw)
	if err != nil {
		t.Fatal(err)
	}
	var page rootScanPage
	if err := json.Unmarshal(env.Body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || !bytes.Contains(page.Records[0], []byte(`"phase":"result"`)) || !bytes.Contains(page.Records[0], event.Body) {
		t.Fatalf("scan lost callback result: %v", err)
	}
}

func TestReleaseCipherCannotSubstituteOriginalEvent(t *testing.T) {
	s, _, _, _ := releaseStore(t)
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finalize(ctx, releaseRequest(t, r, 1, "failed")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE finalizations SET body_cipher=(SELECT body_cipher FROM execution_events WHERE operation_id=? AND event_seq=1),wrapped_dek=(SELECT wrapped_dek FROM execution_events WHERE operation_id=? AND event_seq=1),body_fingerprint=(SELECT body_fingerprint FROM execution_events WHERE operation_id=? AND event_seq=1) WHERE operation_id=?`, r.OperationID, r.OperationID, r.OperationID, r.OperationID); err != nil {
		t.Fatal(err)
	}
	if body, err := s.ReadFinalized(ctx, r.OperationID); !errors.Is(err, ErrEvidence) || len(body) != 0 {
		t.Fatalf("event ciphertext substituted release proof: %v", err)
	}
	if out, err := s.Finalize(ctx, releaseRequest(t, r, 1, "failed")); !errors.Is(err, ErrEvidence) || len(out) != 0 {
		t.Fatalf("substituted receipt replayed: %v", err)
	}
}

func TestReleasePendingBudgetCountsRetainedRecordsOnly(t *testing.T) {
	for _, budget := range []string{"records", "bytes"} {
		for _, keep := range []bool{false, true} {
			name := budget + map[bool]string{false: " empty", true: " unfinished"}[keep]
			t.Run(name, func(t *testing.T) {
				s, _, _, _ := releaseStore(t)
				ctx := context.Background()
				for i := 0; i < 3; i++ {
					prepare := prepareFixture(t)
					operation, err := protocol.NewID()
					if err != nil {
						t.Fatal(err)
					}
					prepare["body"].(map[string]any)["operation_id"] = operation
					r, err := s.RecordReadyFixture(ctx, encodeFixture(t, prepare))
					if err != nil {
						t.Fatal(err)
					}
					if _, err = s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); err != nil {
						t.Fatal(err)
					}
					if _, err = s.Finalize(ctx, releaseRequest(t, r, 1, "failed")); err != nil {
						t.Fatal(err)
					}
					if err = s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, rootScanSession); err != nil {
						t.Fatal(err)
					}
				}
				var wanted []byte
				if keep {
					prepare := prepareFixture(t)
					operation, err := protocol.NewID()
					if err != nil {
						t.Fatal(err)
					}
					prepare["body"].(map[string]any)["operation_id"] = operation
					r, err := s.RecordReadyFixture(ctx, encodeFixture(t, prepare))
					if err != nil {
						t.Fatal(err)
					}
					wanted, err = s.RecoveryRecord(ctx, r.OperationID)
					if err != nil {
						t.Fatal(err)
					}
				}
				if budget == "records" {
					s.recovery.maxRecords = 1
				} else {
					s.recovery.maxBytes = 1
					if keep {
						s.recovery.maxBytes = len(wanted)
					}
				}
				out, err := s.Reconcile(ctx, rootScanRequest(t, "pending", "", "", 200))
				if err != nil {
					t.Fatalf("completed history consumed pending %s budget: %v", budget, err)
				}
				env, err := s.validator.Validate(out)
				if err != nil {
					t.Fatal(err)
				}
				var page rootScanPage
				if err = json.Unmarshal(env.Body, &page); err != nil {
					t.Fatal(err)
				}
				wantCount := uint64(0)
				if keep {
					wantCount = 1
				}
				if page.Total != wantCount || uint64(len(page.Records)) != wantCount {
					t.Fatalf("pending retained %d records want %d", page.Total, wantCount)
				}
				if keep && !bytes.Equal(page.Records[0], wanted) {
					t.Fatal("pending record bytes changed")
				}
				if out, err = s.Reconcile(ctx, rootScanRequest(t, "all", "", "", 200)); !errors.Is(err, ErrScanBusy) || len(out) != 0 {
					t.Fatalf("all escaped %s budget: %v", budget, err)
				}
				replay := rootPage(t, s, rootScanRequest(t, "pending", page.ScanID, "", 200))
				if replay.Total != wantCount {
					t.Fatal("failed all upgrade replaced pending snapshot")
				}
				if keep && !bytes.Equal(replay.Records[0], wanted) {
					t.Fatal("failed all upgrade changed pending bytes")
				}
			})
		}
	}
}

func TestReleasePendingAuthenticatesAcrossIdentityBatches(t *testing.T) {
	s, _, _, _ := releaseStore(t)
	ctx := context.Background()
	var last Record
	// More than one bounded identity batch, with the retained record sorted last.
	for i := 1; i <= 129; i++ {
		prepare := prepareFixture(t)
		prepare["body"].(map[string]any)["operation_id"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		r, err := s.RecordReadyFixture(ctx, encodeFixture(t, prepare))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Finalize(ctx, releaseRequest(t, r, 1, "failed")); err != nil {
			t.Fatal(err)
		}
		if err = s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, rootScanSession); err != nil {
			t.Fatal(err)
		}
		last = r
	}
	prepare := prepareFixture(t)
	prepare["body"].(map[string]any)["operation_id"] = "00000000-0000-4000-8000-999999999999"
	pending, err := s.RecordReadyFixture(ctx, encodeFixture(t, prepare))
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := s.RecoveryRecord(ctx, pending.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	s.recovery.maxRecords = 1
	s.recovery.maxBytes = len(wanted)
	page := rootPage(t, s, rootScanRequest(t, "pending", "", "", 200))
	if page.Total != 1 || len(page.Records) != 1 || !bytes.Equal(page.Records[0], wanted) {
		t.Fatal("later identity batch omitted unfinished record")
	}
	rootComplete(t, s, page.ScanID)
	if _, err = s.db.Exec("UPDATE finalizations SET body_cipher=zeroblob(length(body_cipher)) WHERE operation_id=?", last.OperationID); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Reconcile(ctx, rootScanRequest(t, "pending", "", "", 200)); !errors.Is(err, ErrEvidence) || len(out) != 0 {
		t.Fatalf("later identity batch skipped corrupt omitted history: %v", err)
	}
}
