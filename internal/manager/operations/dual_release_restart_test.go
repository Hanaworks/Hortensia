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

type drCrashMark struct {
	Stage, OperationID string
	Manager            Identity
	Agent              ledger.Identity
	Binding            Binding
	Body               []byte
}

func TestDualReleaseManagerForcedTermination(t *testing.T) {
	for _, stage := range []string{"transaction", "committed"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestDualReleaseCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DM_RELEASE_CRASH="+stage, "DM_RELEASE_DIRECTORY="+dir)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			lines := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(pipe)
				scanner.Buffer(make([]byte, 4096), 1<<20)
				var output strings.Builder
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "RELEASE_BOUNDARY:") {
						lines <- line
						return
					}
					if output.Len() < 65536 {
						output.WriteString(line + "\n")
					}
				}
				lines <- "missing marker: " + output.String()
			}()
			var line string
			select {
			case line = <-lines:
			case <-time.After(25 * time.Second):
				t.Fatal("release helper did not reach boundary")
			}
			if !strings.HasPrefix(line, "RELEASE_BOUNDARY:") {
				_ = cmd.Wait()
				waited = true
				t.Fatalf("%s stderr=%s", line, stderr.String())
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitErr := cmd.Wait()
			waited = true
			if waitErr == nil {
				t.Fatal("helper was not killed")
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "RELEASE_BOUNDARY:"))
			if err != nil {
				t.Fatal(err)
			}
			var mark drCrashMark
			if json.Unmarshal(raw, &mark) != nil || mark.Stage != stage {
				t.Fatal("bad crash marker")
			}
			ctx := context.Background()
			p := &recoveryPair{path: filepath.Join(dir, "manager.db"), agentPath: filepath.Join(dir, "agent.db"), b: mark.Binding}
			p.m, err = OpenRelease(ctx, p.path, mark.Manager, mfKeys())
			if err != nil {
				t.Fatal(err)
			}
			defer p.m.Close()
			p.a, err = ledger.OpenRelease(ctx, p.agentPath, mark.Agent, riAgentKey(), riJournalKey(), ledger.RecoveryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer p.a.Close()
			riFrozen(t, p.m, p.b.NodeID, true)
			got, err := p.m.Get(ctx, mark.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := p.m.ReadFinalized(ctx, mark.OperationID)
			if stage == "transaction" {
				if got.ReleaseStatus != "pending" || readErr == nil {
					t.Fatalf("partial release %+v receipt=%v", got, readErr)
				}
				drClaims(t, p, mark.OperationID, 2, 0)
			} else {
				if got.ReleaseStatus != "released" || readErr != nil || !bytes.Equal(body, mark.Body) {
					t.Fatalf("lost committed receipt %+v %v", got, readErr)
				}
				drClaims(t, p, mark.OperationID, 0, 0)
			}
			p.agentBoot = riReadAgentBoot(t, p.agentPath)
			riBind(t, p)
			riScan(t, p, "all", 10)
			state, err := p.m.ProcessRecovery(ctx, p.b)
			if err != nil || state.Status != "waiting" {
				t.Fatalf("reopen missing current confirmation %+v %v", state, err)
			}
			drFinalize(t, p, mark.OperationID)
			riComplete(t, p)
			drClaims(t, p, mark.OperationID, 0, 0)
		})
	}
}

func TestDualReleaseCrashHelper(t *testing.T) {
	stage := os.Getenv("DM_RELEASE_CRASH")
	if stage == "" {
		return
	}
	ctx := context.Background()
	p := drPairDir(t, os.Getenv("DM_RELEASE_DIRECTORY"))
	op, _ := riPrepare(t, p, 805, true)
	result, err := p.a.RecordResultFixture(ctx, eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "canceled_before_start"))
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, result)
	request, err := p.m.PrepareFinalize(ctx, p.b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := p.a.Finalize(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	mark := drCrashMark{Stage: stage, OperationID: op.OperationID, Manager: p.m.Identity(), Agent: p.a.Identity(), Binding: p.b, Body: mfEnvelope(t, response).Body}
	boundary := func() {
		raw, err := json.Marshal(mark)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("RELEASE_BOUNDARY:" + base64.StdEncoding.EncodeToString(raw))
		for {
			time.Sleep(time.Hour)
		}
	}
	if stage == "transaction" {
		if err = p.m.db.Close(); err != nil {
			t.Fatal(err)
		}
		d := &sqlite.Driver{}
		d.RegisterConnectionHook(verifyConnection)
		if err = d.RegisterFunction("release_crash_pause", &sqlite.FunctionImpl{NArgs: 0, Scalar: func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { boundary(); return nil, nil }}); err != nil {
			t.Fatal(err)
		}
		sql.Register("release-crash-sqlite", d)
		p.m.db, err = sql.Open("release-crash-sqlite", databaseURI(p.path))
		if err != nil {
			t.Fatal(err)
		}
		p.m.db.SetMaxOpenConns(1)
		p.m.db.SetMaxIdleConns(1)
		if _, err = p.m.db.Exec(`CREATE TEMP TRIGGER release_pause AFTER DELETE ON main.resource_claims BEGIN SELECT release_crash_pause(); END`); err != nil {
			t.Fatal(err)
		}
	}
	if err = p.m.ReceiveFinalized(ctx, p.b, response); err != nil {
		t.Fatal(err)
	}
	if stage == "committed" {
		boundary()
	}
	t.Fatal("did not reach release boundary")
}
