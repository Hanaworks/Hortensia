package operations

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/agent/ledger"
	"modernc.org/sqlite"
)

type recoveryCrashMark struct {
	Stage, RoundID, OperationID, AgentBoot    string
	Identity                                  Identity
	AgentIdentity                             ledger.Identity
	Binding                                   Binding
	Payload, Page, CompleteRequest, Completed []byte
}

func TestRecoveryRestartForcedTerminationAtCompletedBoundaries(t *testing.T) {
	for _, stage := range []string{"completed_transaction", "completed_committed"} {
		t.Run(stage, func(t *testing.T) {
			directory := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DM_RECOVERY_CRASH_STAGE="+stage, "DM_RECOVERY_CRASH_DIRECTORY="+directory)
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
				scanner.Buffer(make([]byte, 4096), 1<<20)
				var output strings.Builder
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "RECOVERY_BOUNDARY:") {
						marks <- scanner.Text()
						return
					}
					if output.Len() < 65536 {
						output.WriteString(scanner.Text() + "\n")
					}
				}
				marks <- fmt.Sprintf("missing marker: %s; scanner=%v", output.String(), scanner.Err())
			}()
			var line string
			select {
			case line = <-marks:
			case <-time.After(25 * time.Second):
				t.Fatal("helper did not reach completion boundary")
			}
			if !strings.HasPrefix(line, "RECOVERY_BOUNDARY:") {
				_ = cmd.Wait()
				waited = true
				t.Fatalf("helper failed: %s; stderr=%s", line, stderr.String())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitErr := cmd.Wait()
			waited = true
			if waitErr == nil {
				t.Fatal("helper exited normally instead of forced termination")
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "RECOVERY_BOUNDARY:"))
			if err != nil {
				t.Fatal(err)
			}
			var mark recoveryCrashMark
			if err := json.Unmarshal(raw, &mark); err != nil {
				t.Fatal(err)
			}
			if mark.Stage != stage {
				t.Fatalf("wrong crash boundary: %q", mark.Stage)
			}
			ctx := context.Background()
			m, err := OpenRecovery(ctx, filepath.Join(directory, "manager.db"), mark.Identity, mfKeys())
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			riFrozen(t, m, mark.Binding.NodeID, true)
			// Absence release predates this completion transaction. Both crash
			// positions must preserve its actual all-page proof and zero claims.
			riAssertAbsenceReleased(t, m, mark.OperationID, mark.RoundID)
			payload, err := m.ReadPayload(ctx, mark.OperationID)
			if err != nil || !bytes.Equal(payload, mark.Payload) {
				t.Fatalf("payload changed on crash: %v", err)
			}
			page, err := m.ReadRecoveryPage(ctx, mark.RoundID, 0)
			if err != nil || !bytes.Equal(page, mark.Page) {
				t.Fatalf("absence proof page changed on crash: %v", err)
			}
			var accepted int
			if err := m.db.QueryRow("SELECT accepted FROM recovery_requests WHERE message_id=?", mfEnvelope(t, mark.CompleteRequest).MessageID).Scan(&accepted); err != nil {
				t.Fatal(err)
			}
			want := 0
			if stage == "completed_committed" {
				want = 1
			}
			if accepted != want {
				t.Fatalf("receipt commit=%d want=%d", accepted, want)
			}
			var status string
			if err := m.db.QueryRow("SELECT status FROM recovery_scans WHERE id=?", mark.RoundID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "abandoned" {
				t.Fatalf("reopen retained old round eligibility: %q accepted=%d", status, accepted)
			}
			if mfCount(t, m, "operation_events") != 0 || mfCount(t, m, "recovery_cases") != 0 || mfCount(t, m, "absence_releases") != 1 {
				t.Fatal("crash fabricated or lost business evidence")
			}
			if _, err := m.ReceiveRecoveryCompleted(ctx, mark.Binding, mark.Completed); err == nil {
				t.Fatal("old boot receipt reopened admission")
			}
			riFrozen(t, m, mark.Binding.NodeID, true)
			a, err := ledger.OpenRecovery(ctx, filepath.Join(directory, "agent.db"), mark.AgentIdentity, riAgentKey(), riJournalKey(), ledger.RecoveryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			p := &recoveryPair{m: m, a: a, b: mark.Binding, path: filepath.Join(directory, "manager.db"), agentPath: filepath.Join(directory, "agent.db")}
			p.b.SessionID = riID(9500)
			p.agentBoot = riReadAgentBoot(t, p.agentPath)
			if p.agentBoot == mark.AgentBoot {
				t.Fatal("Agent reopen did not change boot")
			}
			riBind(t, p)
			if _, err := m.ReceiveRecoveryCompleted(ctx, p.b, mark.Completed); err == nil {
				t.Fatal("stale receipt survived current rebind")
			}
			riScan(t, p, "all", 1)
			riComplete(t, p)
			riAssertAbsenceReleased(t, m, mark.OperationID, mark.RoundID)
			if _, err := a.Get(ctx, mark.OperationID); err == nil {
				t.Fatal("recovery replay fabricated an Agent execution")
			}
		})
	}
}

func TestRecoveryCrashHelper(t *testing.T) {
	stage := os.Getenv("DM_RECOVERY_CRASH_STAGE")
	if stage == "" {
		return
	}
	p := riNewPair(t, os.Getenv("DM_RECOVERY_CRASH_DIRECTORY"))
	riScan(t, p, "all", 1)
	riComplete(t, p)
	op, _ := riPrepare(t, p, 20, false)
	scan := riScan(t, p, "all", 1)
	request, completed := riPrepareComplete(t, p)
	riAssertAbsenceReleased(t, p.m, op.OperationID, scan.RoundID)
	payload, err := p.m.ReadPayload(context.Background(), op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := p.m.ReadRecoveryPage(context.Background(), scan.RoundID, 0)
	if err != nil {
		t.Fatal(err)
	}
	mark := recoveryCrashMark{Stage: stage, RoundID: scan.RoundID, OperationID: op.OperationID, AgentBoot: p.agentBoot, Identity: p.m.Identity(), AgentIdentity: p.a.Identity(), Binding: p.b, Payload: payload, Page: page, CompleteRequest: request, Completed: completed}
	boundary := func() {
		raw, err := json.Marshal(mark)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("RECOVERY_BOUNDARY:" + base64.StdEncoding.EncodeToString(raw))
		for {
			time.Sleep(time.Hour)
		}
	}
	if stage == "completed_transaction" {
		// Pause in the unchanged production method's actual SQLite transaction,
		// after it attempts node admission. No production crash hook is added.
		if err := p.m.db.Close(); err != nil {
			t.Fatal(err)
		}
		d := &sqlite.Driver{}
		d.RegisterConnectionHook(verifyConnection)
		if err := d.RegisterFunction("recovery_crash_pause", &sqlite.FunctionImpl{NArgs: 0, Scalar: func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { boundary(); return nil, nil }}); err != nil {
			t.Fatal(err)
		}
		sql.Register("recovery-crash-sqlite", d)
		p.m.db, err = sql.Open("recovery-crash-sqlite", databaseURI(p.path))
		if err != nil {
			t.Fatal(err)
		}
		p.m.db.SetMaxOpenConns(1)
		p.m.db.SetMaxIdleConns(1)
		if _, err := p.m.db.Exec(`CREATE TEMP TRIGGER recovery_pause AFTER UPDATE OF frozen ON main.nodes WHEN OLD.frozen=1 AND NEW.frozen=0 BEGIN SELECT recovery_crash_pause(); END`); err != nil {
			t.Fatal(err)
		}
	}
	if state, err := p.m.ReceiveRecoveryCompleted(context.Background(), p.b, completed); err != nil || state.Status != "completed" {
		t.Fatalf("receive completed: %+v %v", state, err)
	}
	if stage == "completed_committed" {
		boundary()
	}
	t.Fatalf("helper did not reach boundary %q", stage)
}
