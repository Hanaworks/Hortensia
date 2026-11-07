package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

// sfFixtureAt crosses the same initial recovery barrier as a caller. It never
// updates frozen or manufactures a recovery cache directly for fixture setup.
func sfFixtureAt(t *testing.T, path string) (*Store, string) {
	t.Helper()
	s, err := CreateRecovery(context.Background(), path, crashNode, "scan-fault-key", crashKey, recoveryKEK, RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	session := prepareFixture(t)["session_id"].(string)
	if err := s.BindRecoverySessionFixture(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	page := sfScan(t, s, session, "pending", "")
	if _, err := s.CompleteReconcile(context.Background(), sfComplete(t, s, session, page.ScanID)); err != nil {
		t.Fatal(err)
	}
	state, err := s.RecoveryScanState(context.Background())
	if err != nil || state.Frozen {
		t.Fatalf("bootstrap completion did not open admission: %+v %v", state, err)
	}
	return s, session
}

type sfPage struct {
	ScanID   string            `json:"scan_id"`
	Total    uint64            `json:"total"`
	Complete bool              `json:"complete"`
	Records  []json.RawMessage `json:"records"`
}

func sfMessage(t *testing.T, kind, session string, body any) []byte {
	t.Helper()
	raw, err := protocol.NewMessage(kind, session, "", body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sfScanRequest(t *testing.T, session, scope, id string) []byte {
	t.Helper()
	var scan any
	if id != "" {
		scan = id
	}
	return sfMessage(t, "reconcile", session, map[string]any{"scope": scope, "scan_id": scan, "cursor": nil, "limit": 200})
}

func sfComplete(t *testing.T, s *Store, session, id string) []byte {
	t.Helper()
	return sfMessage(t, "reconcile_complete", session, map[string]any{"scan_id": id, "ledger_id": s.Identity().LedgerID})
}

func sfScan(t *testing.T, s *Store, session, scope, id string) sfPage {
	t.Helper()
	raw, err := s.Reconcile(context.Background(), sfScanRequest(t, session, scope, id))
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "reconcile_reply" {
		t.Fatalf("invalid scan reply: %v", err)
	}
	var page sfPage
	if err := json.Unmarshal(env.Body, &page); err != nil || !page.Complete || page.ScanID == "" {
		t.Fatalf("expected enumerated final page: %+v %v", page, err)
	}
	return page
}

// sfRows captures all persisted values relevant to an atomic transition. The
// encryption-use counter is deliberately separate: reservations are never refunded.
func sfRows(t *testing.T, s *Store, table string) []byte {
	t.Helper()
	rows, err := s.db.Query("SELECT * FROM " + table + " ORDER BY 1,2")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	all := make([][]any, 0)
	for rows.Next() {
		values, ptrs := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		all = append(all, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sfInject(t *testing.T, s *Store, on string, commit bool) func() {
	t.Helper()
	statement := "CREATE TEMP TRIGGER sf_fail AFTER " + on + " BEGIN SELECT RAISE(ABORT,'private-scan-secret'); END"
	if commit {
		// This insert is legal until COMMIT checks the deferred foreign key.
		if _, err := s.db.Exec(`CREATE TABLE sf_deferred(parent TEXT REFERENCES executions(operation_id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
			t.Fatal(err)
		}
		statement = "CREATE TEMP TRIGGER sf_fail AFTER " + on + " BEGIN INSERT INTO sf_deferred(parent) VALUES('private-scan-secret'); END"
	}
	if _, err := s.db.Exec(statement); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := s.db.Exec("DROP TRIGGER sf_fail"); err != nil {
			t.Fatal(err)
		}
		if commit {
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM sf_deferred").Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed commit left deferred writes: count=%d err=%v", count, err)
			}
			if _, err := s.db.Exec("DROP TABLE sf_deferred"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestScanStatementAndCommitFailuresAreAtomic(t *testing.T) {
	for _, stage := range []string{"freeze", "members", "change", "completed", "dirty"} {
		for _, commit := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/statement", true: "/commit"}[commit], func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "scan.db")
				s, session := sfFixtureAt(t, path)
				r, q, p := gateReady(t, s)
				starts := 0
				g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) { starts++ })
				if err != nil {
					t.Fatal(err)
				}
				var page sfPage
				if stage != "freeze" {
					page = sfScan(t, s, session, "pending", "")
				}
				result := recoveryResultRaw(t, r, 1, "not_started")
				if stage == "completed" || stage == "dirty" {
					if _, err := s.RecordResultFixture(ctx, result); err != nil {
						t.Fatal(err)
					}
					if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, session); err != nil {
						t.Fatal(err)
					}
				}
				tables := []string{"recovery_control", "recovery_members", "recovery_changes", "executions", "execution_events", "event_state", "event_conflicts", "local_claims"}
				before := make(map[string][]byte, len(tables))
				for _, table := range tables {
					before[table] = sfRows(t, s, table)
				}
				on := map[string]string{
					"freeze":    "UPDATE OF frozen ON main.recovery_control WHEN NEW.frozen=1",
					"members":   "INSERT ON main.recovery_members",
					"change":    "INSERT ON main.recovery_changes",
					"completed": "UPDATE OF completed_scan_id ON main.recovery_control WHEN NEW.completed_scan_id <> OLD.completed_scan_id",
					"dirty":     "UPDATE OF dirty ON main.recovery_control WHEN NEW.dirty=1",
				}[stage]
				remove := sfInject(t, s, on, commit)
				var response []byte
				switch stage {
				case "freeze":
					response, err = s.Reconcile(ctx, sfScanRequest(t, session, "pending", ""))
				case "members":
					response, err = s.Reconcile(ctx, sfScanRequest(t, session, "all", ""))
				case "change":
					var event Event
					event, err = s.RecordResultFixture(ctx, result)
					response = event.Body
				case "completed":
					response, err = s.CompleteReconcile(ctx, sfComplete(t, s, session, page.ScanID))
				case "dirty":
					var event Event
					event, err = s.RecordResultFixture(ctx, bytes.Replace(result, []byte("controlled-secret-result"), []byte("changed-secret-result"), 1))
					response = event.Body
				}
				if !errors.Is(err, ErrStorage) || len(response) != 0 {
					t.Fatalf("fault produced successful evidence: response=%d err=%v", len(response), err)
				}
				if strings.Contains(err.Error(), "private-scan-secret") || strings.Contains(err.Error(), "SQLITE") {
					t.Fatalf("private driver details escaped: %v", err)
				}
				for _, table := range tables {
					if !bytes.Equal(before[table], sfRows(t, s, table)) {
						t.Fatalf("failed %s transaction partially changed %s", stage, table)
					}
				}
				remove()
				if err := g.Authorize(p); err == nil || starts != 0 {
					t.Fatalf("failed scan revived invalidated gate: starts=%d err=%v", starts, err)
				}
				if err := s.commitStart(ctx, p); !errors.Is(err, ErrFrozen) {
					t.Fatalf("failed scan let a fresh start cross barrier: %v", err)
				}
				if stage == "members" {
					replayed := sfScan(t, s, session, "pending", page.ScanID)
					if replayed.ScanID != page.ScanID || replayed.Total != page.Total || !bytes.Equal(replayed.Records[0], page.Records[0]) {
						t.Fatal("failed replacement discarded old snapshot")
					}
				}
				if stage == "completed" {
					if _, err := s.CompleteReconcile(ctx, sfComplete(t, s, session, page.ScanID)); err != nil {
						t.Fatalf("atomic failed completion could not retry: %v", err)
					}
					state, err := s.RecoveryScanState(ctx)
					if err != nil || state.Frozen || state.CompletedScanID != page.ScanID {
						t.Fatalf("successful retry did not complete: %+v %v", state, err)
					}
				}
				identity := s.Identity()
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := OpenRecovery(ctx, path, identity, crashKey, recoveryKEK, RecoveryOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				state, err := reopened.RecoveryScanState(ctx)
				if err != nil || !state.Frozen {
					t.Fatalf("reopen bypassed freeze: %+v %v", state, err)
				}
				gateAssertFacts(t, reopened, r.OperationID, PhaseReady, nil)
			})
		}
	}
}

func TestScanFreezeWaitsForEnteredStartWithoutBlockingReads(t *testing.T) {
	s, session := sfFixtureAt(t, filepath.Join(t.TempDir(), "scan.db"))
	_, q, p := gateReady(t, s)
	ctx := context.Background()
	entered, readNow, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	var readOnce, releaseOnce sync.Once
	t.Cleanup(func() {
		readOnce.Do(func() { close(readNow) })
		releaseOnce.Do(func() { close(release) })
	})
	g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {
		close(entered)
		<-readNow
		r, err := s.Get(ctx, q.Operation)
		if err == nil && r.Phase != PhaseStartCommitted {
			err = errors.New("Start callback cannot see its committed fact")
		}
		claims, claimsErr := s.Claims(ctx, q.Operation)
		if claimsErr != nil {
			err = claimsErr
		} else if len(claims) != 2 {
			err = errors.New("Start callback lost resource claims")
		}
		readDone <- err
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	authorized := make(chan error, 1)
	go func() { authorized <- g.Authorize(p) }()
	gateAwait(t, entered)
	scanned := make(chan error, 1)
	request := sfScanRequest(t, session, "pending", "")
	go func() { _, err := s.Reconcile(ctx, request); scanned <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		frozen := s.recovery.localFrozen
		s.mu.Unlock()
		if frozen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan did not enter local freeze")
		}
		time.Sleep(time.Millisecond)
	}
	readOnce.Do(func() { close(readNow) })
	if err := gateResult(t, readDone); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-scanned:
		t.Fatalf("scan returned while Start callback still running: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := gateResult(t, authorized); err != nil {
		t.Fatal(err)
	}
	if err := gateResult(t, scanned); err != nil {
		t.Fatal(err)
	}
	page := sfScan(t, s, session, "pending", "")
	if page.Total != 1 || len(page.Records) != 1 {
		t.Fatalf("committed start missing from snapshot: %+v", page)
	}
	var record struct {
		Phase string `json:"phase"`
		Last  uint64 `json:"last_event_seq"`
	}
	if err := json.Unmarshal(page.Records[0], &record); err != nil || record.Phase != PhaseStartCommitted || record.Last != 1 {
		t.Fatalf("start winner was not preserved: %+v %v", record, err)
	}
	gateAssertFacts(t, s, p.Operation, PhaseStartCommitted, &p)
}

func TestScanInvalidatesCommittedGateBeforeStart(t *testing.T) {
	s, session := sfFixtureAt(t, filepath.Join(t.TempDir(), "scan.db"))
	_, q, p := gateReady(t, s)
	committed, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	starts := 0
	g := gateWithBarrier(t, s, q, time.Now, func(permit startgate.Permit) error {
		err := s.commitStart(context.Background(), permit)
		close(committed)
		<-release
		return err
	}, func(startgate.Permit) { starts++ })
	done := make(chan error, 1)
	go func() { done <- g.Authorize(p) }()
	gateAwait(t, committed)
	page := sfScan(t, s, session, "pending", "")
	if page.Total != 1 || len(page.Records) != 1 {
		t.Fatalf("scan omitted committed gate: %+v", page)
	}
	var record struct {
		Phase string `json:"phase"`
		Last  uint64 `json:"last_event_seq"`
	}
	if err := json.Unmarshal(page.Records[0], &record); err != nil || record.Phase != PhaseStartCommitted || record.Last != 1 {
		t.Fatalf("scan fabricated not-started after durable commit: %+v %v", record, err)
	}
	once.Do(func() { close(release) })
	if err := gateResult(t, done); !errors.Is(err, startgate.ErrInvalidated) {
		t.Fatalf("scan did not invalidate pending callback: %v", err)
	}
	if starts != 0 {
		t.Fatalf("invalidated committed gate invoked Start %d times", starts)
	}
	gateAssertFacts(t, s, p.Operation, PhaseStartCommitted, &p)
}
