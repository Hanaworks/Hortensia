package service

import (
	"bytes"
	"context"
	"database/sql"
	"docker-manager/internal/agent/compose"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agent/workspace"
	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInitPreservesOrphanedSQLiteMaterials(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		for _, directory := range []bool{false, true} {
			name := suffix + "/file"
			if directory {
				name = suffix + "/directory"
			}
			t.Run(name, func(t *testing.T) {
				c := testConfig(t)
				if e := os.MkdirAll(c.StateDir, 0700); e != nil {
					t.Fatal(e)
				}
				path := filepath.Join(c.StateDir, "agent.db"+suffix)
				original := []byte("orphan evidence must remain byte-for-byte\x00\xff")
				leaf := path
				if directory {
					if e := os.Mkdir(path, 0700); e != nil {
						t.Fatal(e)
					}
					leaf = filepath.Join(path, "evidence")
				}
				if e := os.WriteFile(leaf, original, 0600); e != nil {
					t.Fatal(e)
				}
				if _, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001"); e == nil {
					t.Error("init accepted orphan SQLite material")
				}
				after, e := os.ReadFile(leaf)
				if e != nil || !bytes.Equal(after, original) {
					t.Errorf("orphan material changed: %v", e)
				}
				for _, p := range []string{c.IdentityFile, filepath.Join(c.KeysDir, "ledger.key"), filepath.Join(c.KeysDir, "journal.key"), filepath.Join(c.StateDir, "agent.db")} {
					if _, e := os.Lstat(p); !os.IsNotExist(e) {
						t.Errorf("init created material %s: %v", p, e)
					}
				}
			})
		}
	}
}
func TestInitRejectsIdentityAtSQLiteSidecarBeforeSideEffects(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			c := testConfig(t)
			c.IdentityFile = filepath.Join(c.StateDir, "agent.db"+suffix)
			if _, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001"); e != ErrConfig {
				t.Errorf("reserved SQLite path accepted: %v", e)
			}
			for _, p := range []string{c.StateDir, c.KeysDir} {
				if _, e := os.Lstat(p); !os.IsNotExist(e) {
					t.Errorf("invalid config created %s", p)
				}
			}
		})
	}
}

func TestInitRejectsWindowsSQLitePathCaseAliases(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path alias boundary")
	}
	for _, suffix := range []string{"-WAL", "-SHM", "-JOURNAL"} {
		t.Run(suffix, func(t *testing.T) {
			c := testConfig(t)
			c.IdentityFile = filepath.Join(c.StateDir, "AGENT.DB"+suffix)
			if _, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001"); e != ErrConfig {
				t.Errorf("case alias accepted before side effects: %v", e)
			}
			if _, e := os.Lstat(c.StateDir); !os.IsNotExist(e) {
				t.Error("invalid case alias created state dir")
			}
		})
	}
}
func TestInitRejectsNonV4NodeBeforeSideEffects(t *testing.T) {
	for _, id := range []string{"10000000-0000-1000-8000-000000000001", "10000000-0000-4000-7000-000000000001", "10000000-0000-4000-c000-000000000001", "A0000000-0000-4000-8000-000000000001", "not-an-id"} {
		t.Run(id, func(t *testing.T) {
			c := testConfig(t)
			if _, e := Initialize(context.Background(), c, id); e != ErrConfig {
				t.Errorf("expected preflight ErrConfig, got %v", e)
			}
			for _, p := range []string{c.StateDir, c.KeysDir, filepath.Dir(c.IdentityFile)} {
				if _, e := os.Lstat(p); !os.IsNotExist(e) {
					t.Errorf("invalid node created %s", p)
				}
			}
		})
	}
}

type savedExitFixture struct {
	c                 Config
	id                Identity
	k                 keys
	s                 *ledger.Store
	r                 *runtimeService
	sid, op, commands string
	fact              ledger.ProcessOutcome
	unlock            func() error
}

// Real Store/Materialize/Prepare/Gate APIs; only the external Docker CLI and
// supervisor fact are explicitly controlled, with no production fixture switch.
func newSavedExitFixture(t *testing.T, outcome string) *savedExitFixture {
	t.Helper()
	ctx := context.Background()
	c := testConfig(t)
	id, e := Initialize(ctx, c, "10000000-0000-4000-8000-000000000001")
	if e != nil {
		t.Fatal(e)
	}
	_, k, s, lock, e := openIdentity(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	f := &savedExitFixture{c: c, id: id, k: k, s: s, op: "40000000-0000-4000-8000-000000000001", commands: filepath.Join(t.TempDir(), "commands"), unlock: lock.Close}
	t.Cleanup(func() {
		if f.r != nil {
			f.r.stop()
		}
		f.s.Close()
		clearKeys(&f.k)
		lock.Close()
	})
	p, e := s.RuntimeProfile(ctx)
	if e != nil {
		t.Fatal(e)
	}
	p.EngineID = "fixture-engine"
	p.DockerVersion = "fixture-docker"
	p.ComposeVersion = "fixture-compose"
	p.Fingerprint = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if e = s.BindRuntimeProfile(ctx, p); e != nil {
		t.Fatal(e)
	}
	f.bindSession(t, true)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	f.c.DockerCLI = exe
	eng, e := compose.New(compose.Profile{DockerCLI: exe, DockerSocket: c.DockerSocket, Runtime: p})
	if e != nil {
		t.Fatal(e)
	}
	payload := workspace.Payload{OperationType: "stack.apply", NodeID: id.NodeID, StackID: "20000000-0000-4000-8000-000000000001", VersionID: "30000000-0000-4000-8000-000000000001", ProjectName: "demo", RequiredCapabilities: []string{"compose.apply.v1", "config.bundle.v1"}}
	payload.ExecutionProfile.EndpointID = p.EndpointID
	payload.ExecutionProfile.PolicyRevision = p.PolicyRevision
	payload.Config = workspace.Config{Files: []workspace.File{{Path: "compose.yaml", ContentB64: base64.StdEncoding.EncodeToString([]byte("services:\n  app:\n    image: alpine\n"))}}, ComposeFiles: []string{"compose.yaml"}, Env: map[string]string{"DM_AGENT_SERVICE_CLI_FIXTURE": "1", "DM_AGENT_SERVICE_CLI_RECORD": f.commands, "DM_AGENT_SERVICE_CLI_POSTFILE": f.commands + ".post"}, EnvFiles: []string{}, Profiles: []string{}}
	rawPayload, _ := json.Marshal(payload)
	digest := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	prep := testEnvelope(t, "prepare", f.sid, "", map[string]any{"operation_id": f.op, "input_digest": digest, "node_id": id.NodeID, "stack_id": payload.StackID, "version_id": payload.VersionID, "payload_b64": base64.StdEncoding.EncodeToString(rawPayload)})
	receipt, e := s.ReceivePrepare(ctx, prep.raw)
	if e != nil {
		t.Fatal(e)
	}
	pkg, e := workspace.Materialize(ctx, c.StateDir, workspace.Input{OperationID: f.op, InputFingerprint: receipt.Record.LocalFingerprint, Payload: rawPayload})
	if e != nil {
		t.Fatal(e)
	}
	prepared, e := eng.Prepare(ctx, pkg, ledger.ProjectBinding{})
	if e != nil {
		calls, _ := os.ReadFile(f.commands)
		t.Fatalf("prepare controlled recovery fixture: %v; completed CLI calls:\n%s", e, calls)
	}
	if _, e = s.CommitPrepared(ctx, f.op, prepared.Evidence); e != nil {
		t.Fatal(e)
	}
	nonce, _ := protocol.NewID()
	mid, _ := protocol.NewID()
	pid, _ := protocol.NewID()
	gate, e := s.NewGate(ctx, startgate.Request{Session: f.sid, Operation: f.op, Digest: digest, Nonce: nonce, MessageID: mid}, time.Now, func(startgate.Permit) {})
	if e != nil {
		t.Fatal(e)
	}
	if e = gate.Authorize(startgate.Permit{Session: f.sid, Operation: f.op, Digest: digest, Nonce: nonce, PermitID: pid, AuthRev: 1, ReplyTo: mid}); e != nil {
		t.Fatal(e)
	}
	if e = s.RecordProcessCreated(ctx, f.op, 12345); e != nil {
		t.Fatal(e)
	}
	exit := 17
	f.fact = ledger.ProcessOutcome{BootID: s.BootID(), Outcome: outcome, ExitCode: &exit, FinishedAt: "2026-11-12T01:23:45.678Z", Output: []byte("original output\n保留\x00"), OutputTruncated: true}
	if outcome == "interrupted" {
		f.fact.InterruptReason = "agent_stopping"
		f.fact.ErrorCode = "agent_stopping"
	}
	if e = s.RecordProcessExit(ctx, f.op, f.fact); e != nil {
		t.Fatal(e)
	}
	life, stop := context.WithCancel(ctx)
	f.r = &runtimeService{c: f.c, s: s, engine: eng, readable: true, ctx: life, stop: stop, ops: map[string]*operation{}, queued: map[string]bool{}, pages: newPageCache(time.Now)}
	return f
}
func (f *savedExitFixture) bindSession(t *testing.T, complete bool) {
	t.Helper()
	ctx := context.Background()
	f.sid, _ = protocol.NewID()
	p, e := f.s.RuntimeProfile(ctx)
	if e != nil {
		t.Fatal(e)
	}
	_, hid, e := hello(f.s, p, nil)
	if e != nil {
		t.Fatal(e)
	}
	welcome := testEnvelope(t, "welcome", f.sid, hid, map[string]any{"node_id": f.id.NodeID, "session_id": f.sid, "reconcile_required": true})
	if e = f.s.BindManagerSession(ctx, welcome.raw, hid); e != nil {
		t.Fatal(e)
	}
	if !complete {
		return
	}
	scan := testEnvelope(t, "reconcile", f.sid, "", map[string]any{"scope": "pending", "scan_id": nil, "cursor": nil, "limit": 200})
	raw, e := f.s.Reconcile(ctx, scan.raw)
	if e != nil {
		t.Fatal(e)
	}
	var page struct {
		Body struct {
			ScanID string `json:"scan_id"`
		} `json:"body"`
	}
	json.Unmarshal(raw, &page)
	done := testEnvelope(t, "reconcile_complete", f.sid, "", map[string]any{"scan_id": page.Body.ScanID, "ledger_id": f.id.LedgerID})
	if _, e = f.s.CompleteReconcile(ctx, done.raw); e != nil {
		t.Fatal(e)
	}
}
func (f *savedExitFixture) reopen(t *testing.T) {
	t.Helper()
	if e := f.s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e := ledger.OpenRuntime(context.Background(), filepath.Join(f.c.StateDir, "agent.db"), f.id.ledgerIdentity(), f.k.ledger, f.k.journal, ledger.RuntimeOptions{})
	if e != nil {
		t.Fatal(e)
	}
	f.s = s
	f.r.s = s
	f.r.ops = map[string]*operation{}
	f.r.pages = newPageCache(time.Now)
	if s.BootID() == f.fact.BootID {
		t.Fatal("reopen reused boot")
	}
	f.bindSession(t, false)
}
func (f *savedExitFixture) observe(t *testing.T) []byte {
	t.Helper()
	qid, _ := protocol.NewID()
	m := testEnvelope(t, "observe", f.sid, "", map[string]any{"query_id": qid, "operation_id": f.op, "stack_id": "20000000-0000-4000-8000-000000000001", "resource_kind": "container", "cursor": nil, "limit": 1})
	ss := &session{id: f.sid, ctx: context.Background(), cancel: func() {}, out: make(chan []byte, 32)}
	f.r.observe(ss, m)
	select {
	case raw := <-ss.out:
		return raw
	case <-time.After(5 * time.Second):
		t.Fatal("missing observe response")
		return nil
	}
}
func (f *savedExitFixture) assertResult(t *testing.T, seq uint64) []byte {
	t.Helper()
	event, e := f.s.ReadEvent(context.Background(), f.op, seq)
	if e != nil {
		t.Fatalf("stored exit was not recovered into result seq%d: %v", seq, e)
	}
	var b struct {
		Outcome   string `json:"outcome"`
		Exit      *int   `json:"exit_code"`
		Finished  string `json:"finished_at"`
		Output    string `json:"output_b64"`
		Truncated bool   `json:"output_truncated"`
	}
	if json.Unmarshal(event.Body, &b) != nil || b.Outcome != f.fact.Outcome || b.Exit == nil || *b.Exit != 17 || b.Finished != f.fact.FinishedAt || b.Output != base64.StdEncoding.EncodeToString(f.fact.Output) || !b.Truncated {
		t.Fatalf("result changed stored fact: %s", event.Body)
	}
	fact, e := f.s.ReadProcessOutcome(context.Background(), f.op)
	saved, _ := json.Marshal(f.fact)
	actual, _ := json.Marshal(fact)
	if e != nil || !bytes.Equal(saved, actual) {
		t.Fatalf("durable process bytes changed: %v", e)
	}
	calls, _ := os.ReadFile(f.commands)
	if strings.Contains(string(calls), "up --detach") {
		t.Fatal("recovery restarted old command")
	}
	claims, e := f.s.Claims(context.Background(), f.op)
	if e != nil || len(claims) != 2 {
		t.Fatal("recovery released claims", e)
	}
	return event.Body
}
func TestSavedExitReopenedObserveRecoversOriginalResult(t *testing.T) {
	f := newSavedExitFixture(t, "normal_exit")
	f.reopen(t)
	reply := f.observe(t)
	f.assertResult(t, 2)
	if _, e := f.s.ReadEvent(context.Background(), f.op, 3); e != nil {
		t.Fatalf("recovered normal exit cannot accept supplemental observation: %s; %v", reply, e)
	}
	f.assertResult(t, 3)
}
func TestSavedExitSameBootResultWriteFailureCanRetry(t *testing.T) {
	f := newSavedExitFixture(t, "normal_exit")
	db, e := sql.Open("sqlite", filepath.Join(f.c.StateDir, "agent.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`CREATE TRIGGER fail_result BEFORE INSERT ON execution_events WHEN NEW.kind='result' BEGIN SELECT RAISE(FAIL,'temporary result write failure'); END`); e != nil {
		t.Fatal(e)
	}
	f.observe(t)
	if _, e = f.s.ReadEvent(context.Background(), f.op, 2); e != ledger.ErrNotFound {
		t.Fatal("write fault produced result", e)
	}
	if _, e = db.Exec(`DROP TRIGGER fail_result`); e != nil {
		t.Fatal(e)
	}
	f.observe(t)
	f.assertResult(t, 2)
}

func TestOpenIdentityRejectsJournalDirectoryBeforeSQLite(t *testing.T) {
	c := testConfig(t)
	if _, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001"); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(c.StateDir, "agent.db-journal")
	if e := os.Mkdir(path, 0700); e != nil {
		t.Fatal(e)
	}
	original := []byte("preserve nested journal evidence")
	leaf := filepath.Join(path, "evidence")
	if e := os.WriteFile(leaf, original, 0600); e != nil {
		t.Fatal(e)
	}
	_, k, s, lock, e := openIdentity(context.Background(), c)
	clearKeys(&k)
	if s != nil {
		s.Close()
	}
	if lock != nil {
		lock.Close()
	}
	if e != ErrIdentity {
		t.Errorf("unsafe journal was not rejected before SQLite: %v", e)
	}
	after, re := os.ReadFile(leaf)
	if re != nil || !bytes.Equal(after, original) {
		t.Errorf("journal material changed: %v", re)
	}
}
func TestInitAndOpenIdentityRejectSidecarLinks(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe")
	target := filepath.Join(t.TempDir(), "target")
	if e := os.WriteFile(target, []byte("linked original material"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, probe); e != nil {
		t.Skipf("symbolic links unavailable: %v", e)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		for _, open := range []bool{false, true} {
			t.Run(suffix+map[bool]string{false: "/init", true: "/open"}[open], func(t *testing.T) {
				c := testConfig(t)
				if open {
					if _, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001"); e != nil {
						t.Fatal(e)
					}
				} else if e := os.MkdirAll(c.StateDir, 0700); e != nil {
					t.Fatal(e)
				}
				path := filepath.Join(c.StateDir, "agent.db"+suffix)
				if e := os.Symlink(target, path); e != nil {
					t.Fatal(e)
				}
				if open {
					_, k, s, lock, e := openIdentity(context.Background(), c)
					clearKeys(&k)
					if s != nil {
						s.Close()
					}
					if lock != nil {
						lock.Close()
					}
					if e != ErrIdentity {
						t.Errorf("open accepted linked sidecar: %v", e)
					}
				} else if _, e := Initialize(context.Background(), c, "10000000-0000-4000-8000-000000000001"); e == nil {
					t.Error("init accepted linked sidecar")
				}
				info, e := os.Lstat(path)
				if e != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("sidecar link changed: %v", e)
				}
				after, e := os.ReadFile(target)
				if e != nil || string(after) != "linked original material" {
					t.Errorf("target material changed: %v", e)
				}
			})
		}
	}
}

func TestSavedExitRecoveryRetriesWithoutSession(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		for _, outcome := range []string{"normal_exit", "interrupted"} {
			name := outcome + map[bool]string{false: "/same_boot", true: "/reopened"}[reopen]
			t.Run(name, func(t *testing.T) {
				f := newSavedExitFixture(t, outcome)
				if reopen {
					f.reopen(t)
				}
				if e := f.s.DisconnectManagerSession(context.Background(), f.sid); e != nil {
					t.Fatal(e)
				}
				if e := f.r.recoverPendingOutcomes(); e != nil {
					t.Fatal(e)
				}
				original := f.assertResult(t, 2)
				if e := f.r.recoverPendingOutcomes(); e != nil {
					t.Fatal(e)
				}
				repeated := f.assertResult(t, 2)
				if !bytes.Equal(original, repeated) {
					t.Fatal("retry rewrote result")
				}
				if _, e := f.s.ReadEvent(context.Background(), f.op, 3); e != ledger.ErrNotFound {
					t.Fatal("retry added another event", e)
				}
			})
		}
	}
}
func TestSavedExitRecoveryAfterTransientWriteFailure(t *testing.T) {
	f := newSavedExitFixture(t, "normal_exit")
	db, e := sql.Open("sqlite", filepath.Join(f.c.StateDir, "agent.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`CREATE TRIGGER fail_result BEFORE INSERT ON execution_events WHEN NEW.kind='result' BEGIN SELECT RAISE(FAIL,'temporary result write failure'); END`); e != nil {
		t.Fatal(e)
	}
	if e = f.r.recoverPendingOutcomes(); e == nil {
		t.Error("result write failure was not reported")
	}
	if _, e = f.s.ReadEvent(context.Background(), f.op, 2); e != ledger.ErrNotFound {
		t.Fatal("fault fabricated result", e)
	}
	claims, e := f.s.Claims(context.Background(), f.op)
	if e != nil || len(claims) != 2 {
		t.Fatal("fault released claims", e)
	}
	actual, e := f.s.ReadProcessOutcome(context.Background(), f.op)
	if e != nil || !bytes.Equal(actual.Output, f.fact.Output) {
		t.Fatal("fault lost durable process fact", e)
	}
	if _, e = db.Exec(`DROP TRIGGER fail_result`); e != nil {
		t.Fatal(e)
	}
	if e = f.r.recoverPendingOutcomes(); e != nil {
		t.Fatal(e)
	}
	f.assertResult(t, 2)
}

func TestServeRecoversSavedExitAndRetriesWhileDisconnected(t *testing.T) {
	for _, tc := range []struct {
		name           string
		fault, corrupt bool
	}{{"startup", false, false}, {"transient_write", true, false}, {"permanent_corruption", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			fault := tc.fault
			f := newSavedExitFixture(t, "normal_exit")
			f.id.ActiveSecret = f.id.CandidateSecret
			f.id.CandidateSecret = ""
			f.id.CredentialGeneration = 1
			f.id.CredentialState = "active"
			if e := saveIdentity(f.c, f.id); e != nil {
				t.Fatal(e)
			}
			if e := f.s.Close(); e != nil {
				t.Fatal(e)
			}
			if e := f.unlock(); e != nil {
				t.Fatal(e)
			}
			db, e := sql.Open("sqlite", filepath.Join(f.c.StateDir, "agent.db"))
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			barrier := filepath.Join(t.TempDir(), "probe-release")
			if fault {
				// Probe deliberately ignores ambient environment. A private copy of
				// this test executable carries its own test-only barrier sidecar.
				executable, e := os.Executable()
				if e != nil {
					t.Fatal(e)
				}
				raw, e := os.ReadFile(executable)
				if e != nil {
					t.Fatal(e)
				}
				f.c.DockerCLI = filepath.Join(t.TempDir(), filepath.Base(executable))
				if e = os.WriteFile(f.c.DockerCLI, raw, 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(f.c.DockerCLI+".probe-barrier", []byte(barrier), 0600); e != nil {
					t.Fatal(e)
				}
			}
			// No HTTP listener: recovery cannot depend on a connected Manager/session.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, f.c) }()
			if fault {
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, e := os.Stat(barrier + ".entered"); e == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("serve never reached post-open probe")
					}
					time.Sleep(10 * time.Millisecond)
				}
				if _, e = db.Exec(`CREATE TRIGGER fail_result BEFORE INSERT ON execution_events WHEN NEW.kind='result' BEGIN SELECT RAISE(FAIL,'temporary result write failure'); END`); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(barrier, []byte("release"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			resultCount := func() int {
				t.Helper()
				var count int
				if e := db.QueryRow(`SELECT count(*) FROM execution_events WHERE operation_id=? AND event_seq=2`, f.op).Scan(&count); e != nil {
					t.Fatal(e)
				}
				return count
			}
			if fault {
				time.Sleep(1200 * time.Millisecond)
				if resultCount() != 0 {
					t.Fatal("write fault produced a result")
				}
				if tc.corrupt {
					if _, e = db.Exec(`UPDATE runtime_documents SET body_cipher=zeroblob(length(body_cipher)) WHERE operation_id=? AND kind='process'`, f.op); e != nil {
						t.Fatal(e)
					}
				}
				if _, e = db.Exec(`DROP TRIGGER fail_result`); e != nil {
					t.Fatal(e)
				}
				if tc.corrupt {
					select {
					case e := <-done:
						if e == nil || !strings.HasPrefix(e.Error(), "ledger ") {
							t.Fatalf("Serve did not expose a fixed ledger diagnostic: %v", e)
						}
					case <-time.After(4 * time.Second):
						t.Fatal("Serve swallowed permanent recovery error")
					}
					if resultCount() != 0 {
						t.Fatal("corruption fabricated a result")
					}
					var claims int
					if e = db.QueryRow(`SELECT count(*) FROM local_claims WHERE operation_id=?`, f.op).Scan(&claims); e != nil || claims != 2 {
						t.Fatal("corruption released claims", e)
					}
					return
				}
			}
			deadline := time.Now().Add(4 * time.Second)
			for resultCount() == 0 && time.Now().Before(deadline) {
				select {
				case e := <-done:
					t.Fatalf("serve stopped before recovery: %v", e)
				case <-time.After(20 * time.Millisecond):
				}
			}
			count := resultCount()
			cancel()
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("serve did not stop")
			}
			if count != 1 {
				t.Fatal("serve never recovered durable exit")
			}
			s, e := ledger.OpenRuntime(context.Background(), filepath.Join(f.c.StateDir, "agent.db"), f.id.ledgerIdentity(), f.k.ledger, f.k.journal, ledger.RuntimeOptions{})
			if e != nil {
				t.Fatal(e)
			}
			f.s = s
			f.r.s = s
			f.assertResult(t, 2)
		})
	}
}
func TestOpenIdentityAllowsExistingPrivateWAL(t *testing.T) {
	c := testConfig(t)
	ctx := context.Background()
	if _, e := Initialize(ctx, c, "10000000-0000-4000-8000-000000000001"); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", filepath.Join(c.StateDir, "agent.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`PRAGMA journal_mode=WAL`); e != nil {
		t.Fatal(e)
	}
	// Existing schema remains exact; a valid profile metadata write retains
	// a populated WAL while this second SQLite connection stays open.
	if _, e = db.Exec(`UPDATE runtime_profile SET policy_revision=7 WHERE id=1`); e != nil {
		t.Fatal(e)
	}
	wal := filepath.Join(c.StateDir, "agent.db-wal")
	if info, e := os.Stat(wal); e != nil || info.Size() == 0 {
		t.Fatal("test did not create a populated WAL", e)
	}
	if e = os.Chmod(wal, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(filepath.Join(c.StateDir, "agent.db-shm"), 0600); e != nil {
		t.Fatal(e)
	}
	_, k, s, lock, e := openIdentity(ctx, c)
	defer clearKeys(&k)
	if e != nil {
		t.Fatal("valid WAL rejected", e)
	}
	defer s.Close()
	defer lock.Close()
	profile, e := s.RuntimeProfile(ctx)
	if e != nil || profile.PolicyRevision != 7 {
		t.Fatal("WAL data lost", e)
	}
}

func TestOutcomeRecoveryStopsOnCorruptDurableExit(t *testing.T) {
	f := newSavedExitFixture(t, "normal_exit")
	db, e := sql.Open("sqlite", filepath.Join(f.c.StateDir, "agent.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`UPDATE runtime_documents SET body_cipher=zeroblob(length(body_cipher)) WHERE operation_id=? AND kind='process'`, f.op); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() { f.r.runOutcomeRecovery(); close(done) }()
	defer func() { f.r.stop(); <-done }()
	select {
	case <-done:
	case <-time.After(1800 * time.Millisecond):
		t.Error("recovery silently kept retrying corrupt authenticated evidence")
	}
	if _, e = f.s.ReadEvent(context.Background(), f.op, 2); e != ledger.ErrNotFound {
		t.Fatal("corruption produced result", e)
	}
	claims, e := f.s.Claims(context.Background(), f.op)
	if e != nil || len(claims) != 2 {
		t.Fatal("corruption released claims", e)
	}
}
func TestOutcomeRecoveryDoesNotPreemptLiveSupervisor(t *testing.T) {
	f := newSavedExitFixture(t, "normal_exit")
	f.r.ops[f.op] = &operation{supervised: true, collecting: true}
	if e := f.r.recoverPendingOutcomes(); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.ReadEvent(context.Background(), f.op, 2); e != ledger.ErrNotFound {
		t.Fatal("recovery preempted live observation", e)
	}
	f.r.mu.Lock()
	f.r.ops[f.op].collecting = false
	f.r.mu.Unlock()
	if e := f.r.recoverPendingOutcomes(); e != nil {
		t.Fatal(e)
	}
	f.assertResult(t, 2)
}
