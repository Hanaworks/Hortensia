package ledger

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
	"modernc.org/sqlite"
)

type sfCrashMark struct {
	Stage                              string
	Identity                           Identity
	Session, ScanID, PreviousCompleted string
	CompletedBody, Result              []byte
}

func TestScanForcedTerminationAtCompletionBoundaries(t *testing.T) {
	for _, stage := range []string{"completed_transaction", "completed_before_receipt"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "scan.db")
			cmd := exec.Command(os.Args[0], "-test.run=^TestScanCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DM_SCAN_STAGE="+stage, "DM_SCAN_PATH="+path)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			marks := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(pipe)
				var output strings.Builder
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "SCAN_BOUNDARY:") {
						marks <- scanner.Text()
						return
					}
					if output.Len() < 65536 {
						output.WriteString(scanner.Text() + "\n")
					}
				}
				marks <- "missing marker: " + output.String()
			}()
			var line string
			select {
			case line = <-marks:
			case <-time.After(25 * time.Second):
				t.Fatal("helper did not reach actual completion boundary")
			}
			if !strings.HasPrefix(line, "SCAN_BOUNDARY:") {
				_ = cmd.Wait()
				waited = true
				t.Fatalf("helper failed: %s; %s", line, stderr.String())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitErr := cmd.Wait()
			waited = true
			if waitErr == nil {
				t.Fatal("helper completed normally instead of being forcibly terminated")
			}
			encoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "SCAN_BOUNDARY:"))
			if err != nil {
				t.Fatal(err)
			}
			var mark sfCrashMark
			if err := json.Unmarshal(encoded, &mark); err != nil || mark.Stage != stage {
				t.Fatalf("wrong crash marker: %+v %v", mark, err)
			}
			ctx := context.Background()
			s, err := OpenRecovery(ctx, path, mark.Identity, crashKey, recoveryKEK, RecoveryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			state, err := s.RecoveryScanState(ctx)
			if err != nil || !state.Frozen {
				t.Fatalf("process reopen did not retain recovery barrier: %+v %v", state, err)
			}
			var members, changes int
			if err := s.db.QueryRow("SELECT count(*) FROM recovery_members WHERE scan_id=?", mark.ScanID).Scan(&members); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow("SELECT count(*) FROM recovery_changes").Scan(&changes); err != nil {
				t.Fatal(err)
			}
			if members != 1 {
				t.Fatalf("fixed identity set lost on crash: %d", members)
			}
			if err := s.BindRecoverySessionFixture(ctx, mark.Session); err != nil {
				t.Fatal(err)
			}
			request := sfComplete(t, s, mark.Session, mark.ScanID)
			reply, err := s.CompleteReconcile(ctx, request)
			if stage == "completed_transaction" {
				if state.CompletedScanID != mark.PreviousCompleted || state.ActiveScanID != mark.ScanID || changes != 1 {
					t.Fatalf("uncommitted completion survived or discarded changes: state=%+v changes=%d", state, changes)
				}
				if !errors.Is(err, ErrScanExpired) || len(reply) != 0 {
					t.Fatalf("uncommitted completion replayed: response=%d err=%v", len(reply), err)
				}
			} else {
				if state.CompletedScanID != mark.ScanID || state.ActiveScanID != "" || changes != 0 {
					t.Fatalf("committed completion missing: state=%+v changes=%d", state, changes)
				}
				if err != nil {
					t.Fatal(err)
				}
				completed, err := s.validator.Validate(reply)
				if err != nil || completed.Type != "reconcile_completed" || !bytes.Equal(completed.Body, mark.CompletedBody) {
					t.Fatalf("durable completion body changed: %v", err)
				}
				var sent protocol.Envelope
				if err := json.Unmarshal(request, &sent); err != nil {
					t.Fatal(err)
				}
				if completed.ReplyTo == nil || *completed.ReplyTo != sent.MessageID {
					t.Fatal("replayed completion lost current request correlation")
				}
				state, err = s.RecoveryScanState(ctx)
				if err != nil || !state.Frozen {
					t.Fatalf("historical receipt reopened admission after boot change: %+v %v", state, err)
				}
				// A repeated old receipt must also leave a newly active scan alone.
				fresh := sfScan(t, s, mark.Session, "all", "")
				replay, err := s.CompleteReconcile(ctx, sfComplete(t, s, mark.Session, mark.ScanID))
				if err != nil {
					t.Fatal(err)
				}
				env, err := s.validator.Validate(replay)
				if err != nil || !bytes.Equal(env.Body, mark.CompletedBody) {
					t.Fatalf("old completion no longer replayable during new scan: %v", err)
				}
				state, err = s.RecoveryScanState(ctx)
				if err != nil || !state.Frozen || state.ActiveScanID != fresh.ScanID {
					t.Fatalf("old receipt completed a different scan: %+v %v", state, err)
				}
			}
			_, q, _ := crashPrepare(t)
			result, err := s.ReadEvent(ctx, q.Operation, 1)
			if err != nil || !bytes.Equal(result.Body, mark.Result) || result.AckSession != mark.Session {
				t.Fatalf("crash altered committed result or acknowledgement: %v", err)
			}
			gateAssertFacts(t, s, q.Operation, PhaseReady, nil)
			if _, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) { t.Error("restarted pre-crash task") }); !errors.Is(err, ErrFrozen) {
				t.Fatalf("old gate accepted after crash: %v", err)
			}
			prepare, _, _ := crashPrepare(t)
			if _, err := s.RecordReadyFixture(ctx, prepare); !errors.Is(err, ErrFrozen) {
				t.Fatalf("historical receipt allowed prepare: %v", err)
			}
		})
	}
}

func TestScanCrashHelper(t *testing.T) {
	stage := os.Getenv("DM_SCAN_STAGE")
	if stage == "" {
		return
	}
	path := os.Getenv("DM_SCAN_PATH")
	s, session := sfFixtureAt(t, path)
	// Cleanup cannot run: the parent kills this process at the marker.
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	previous, err := s.RecoveryScanState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	page := sfScan(t, s, session, "pending", "")
	event, err := s.RecordResultFixture(ctx, recoveryResultRaw(t, r, 1, "not_started"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, session); err != nil {
		t.Fatal(err)
	}
	mark := sfCrashMark{Stage: stage, Identity: s.Identity(), Session: session, ScanID: page.ScanID, PreviousCompleted: previous.CompletedScanID, Result: event.Body}
	boundary := func() {
		encoded, err := json.Marshal(mark)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("SCAN_BOUNDARY:" + base64.StdEncoding.EncodeToString(encoded))
		for {
			time.Sleep(time.Hour)
		}
	}
	if stage == "completed_transaction" {
		// Replace only the driver connection, retaining the Store, actual cache,
		// and production CompleteReconcile. A TEMP trigger pauses the real write
		// transaction after its control update and before COMMIT.
		if err := s.db.Close(); err != nil {
			t.Fatal(err)
		}
		d := &sqlite.Driver{}
		d.RegisterConnectionHook(verifyConnection)
		if err := d.RegisterFunction("sf_crash_pause", &sqlite.FunctionImpl{NArgs: 0, Scalar: func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { boundary(); return nil, nil }}); err != nil {
			t.Fatal(err)
		}
		sql.Register("scan-crash-sqlite", d)
		s.db, err = sql.Open("scan-crash-sqlite", databaseURI(path))
		if err != nil {
			t.Fatal(err)
		}
		s.db.SetMaxOpenConns(1)
		s.db.SetMaxIdleConns(1)
		if _, err := s.db.Exec(`CREATE TEMP TRIGGER sf_pause AFTER UPDATE OF completed_scan_id ON main.recovery_control WHEN NEW.completed_scan_id <> OLD.completed_scan_id BEGIN SELECT sf_crash_pause(); END`); err != nil {
			t.Fatal(err)
		}
	}
	reply, err := s.CompleteReconcile(ctx, sfComplete(t, s, session, page.ScanID))
	if err != nil {
		t.Fatal(err)
	}
	if stage == "completed_before_receipt" {
		env, err := s.validator.Validate(reply)
		if err != nil {
			t.Fatal(err)
		}
		mark.CompletedBody = env.Body
		// No transport or Manager receipt occurs. The local commit has returned;
		// the parent kills the process before this response could be delivered.
		boundary()
	}
	t.Fatalf("helper did not reach requested boundary %q", stage)
}
