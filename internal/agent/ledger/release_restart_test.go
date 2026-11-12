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

	"modernc.org/sqlite"
)

type releaseCrashMark struct {
	Stage           string
	Identity        Identity
	Record          Record
	Result, Receipt []byte
}

func TestReleaseForcedTermination(t *testing.T) {
	for _, stage := range []string{"transaction", "committed_before_delivery"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release.db")
			cmd := exec.Command(os.Args[0], "-test.run=^TestReleaseCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DM_RELEASE_STAGE="+stage, "DM_RELEASE_PATH="+path)
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
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			marks := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(pipe)
				var output strings.Builder
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "RELEASE_BOUNDARY:") {
						marks <- line
						return
					}
					if output.Len() < 65536 {
						output.WriteString(line + "\n")
					}
				}
				marks <- output.String()
			}()
			var line string
			select {
			case line = <-marks:
			case <-time.After(25 * time.Second):
				t.Fatal("release helper did not reach boundary")
			}
			if !strings.HasPrefix(line, "RELEASE_BOUNDARY:") {
				cmd.Wait()
				waited = true
				t.Fatalf("helper failed %s %s", line, stderr.String())
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			if err == nil {
				t.Fatal("process exited normally")
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "RELEASE_BOUNDARY:"))
			if err != nil {
				t.Fatal(err)
			}
			var mark releaseCrashMark
			if err = json.Unmarshal(raw, &mark); err != nil || mark.Stage != stage {
				t.Fatal("bad boundary mark")
			}
			ctx := context.Background()
			s, err := OpenRelease(ctx, path, mark.Identity, crashKey, recoveryKEK, RecoveryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			state, err := s.RecoveryScanState(ctx)
			if err != nil || !state.Frozen {
				t.Fatalf("reopen not frozen: %v", err)
			}
			claims, err := s.Claims(ctx, mark.Record.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			receipt, readErr := s.ReadFinalized(ctx, mark.Record.OperationID)
			if stage == "transaction" {
				if len(claims) != 2 || !errors.Is(readErr, ErrNotFound) || len(receipt) != 0 {
					t.Fatalf("uncommitted release survived: claims=%v err=%v", claims, readErr)
				}
			} else if len(claims) != 0 || readErr != nil || !bytes.Equal(receipt, mark.Receipt) {
				t.Fatalf("committed evidence missing: claims=%v err=%v", claims, readErr)
			}
			event, err := s.ReadEvent(ctx, mark.Record.OperationID, 1)
			if err != nil || !bytes.Equal(event.Body, mark.Result) {
				t.Fatal("crash changed original result")
			}
			if err = s.BindRecoverySessionFixture(ctx, rootScanSession); err != nil {
				t.Fatal(err)
			}
			response, err := s.Finalize(ctx, releaseRequest(t, mark.Record, 1, "failed"))
			if err != nil {
				t.Fatal(err)
			}
			env, err := s.validator.Validate(response)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err = s.ReadFinalized(ctx, mark.Record.OperationID)
			if err != nil || !bytes.Equal(receipt, env.Body) {
				t.Fatal("retry receipt differs")
			}
			claims, err = s.Claims(ctx, mark.Record.OperationID)
			if err != nil || len(claims) != 0 {
				t.Fatal("retry did not converge")
			}
			state, err = s.RecoveryScanState(ctx)
			if err != nil || !state.Frozen {
				t.Fatal("finalize thawed reopened ledger")
			}
		})
	}
}

func TestReleaseCrashHelper(t *testing.T) {
	stage := os.Getenv("DM_RELEASE_STAGE")
	if stage == "" {
		return
	}
	path := os.Getenv("DM_RELEASE_PATH")
	ctx := context.Background()
	s, err := CreateRelease(ctx, path, testNode, "release-crash-key", crashKey, recoveryKEK, RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindRecoverySessionFixture(ctx, rootScanSession); err != nil {
		t.Fatal(err)
	}
	rootEnable(t, s)
	r, _, _ := gateReady(t, s)
	event, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1))
	if err != nil {
		t.Fatal(err)
	}
	mark := releaseCrashMark{Stage: stage, Identity: s.Identity(), Record: r, Result: event.Body}
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
		if err = s.db.Close(); err != nil {
			t.Fatal(err)
		}
		d := &sqlite.Driver{}
		d.RegisterConnectionHook(verifyConnection)
		if err = d.RegisterFunction("release_crash_pause", &sqlite.FunctionImpl{NArgs: 0, Scalar: func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { boundary(); return nil, nil }}); err != nil {
			t.Fatal(err)
		}
		sql.Register("release-crash-sqlite", d)
		s.db, err = sql.Open("release-crash-sqlite", databaseURI(path))
		if err != nil {
			t.Fatal(err)
		}
		s.db.SetMaxOpenConns(1)
		s.db.SetMaxIdleConns(1)
		// A temporary trigger stops the production transaction after its last claim
		// deletion, before COMMIT. No persisted schema mutation or production hook.
		if _, err = s.db.Exec(`CREATE TEMP TRIGGER release_pause AFTER DELETE ON main.local_claims WHEN (SELECT count(*) FROM main.local_claims WHERE operation_id=OLD.operation_id)=0 BEGIN SELECT release_crash_pause(); END`); err != nil {
			t.Fatal(err)
		}
	}
	response, err := s.Finalize(ctx, releaseRequest(t, r, 1, "failed"))
	if err != nil {
		t.Fatal(err)
	}
	if stage == "committed_before_delivery" {
		env, err := s.validator.Validate(response)
		if err != nil {
			t.Fatal(err)
		}
		mark.Receipt = env.Body
		boundary()
	}
	t.Fatal("unexpected release boundary")
}
