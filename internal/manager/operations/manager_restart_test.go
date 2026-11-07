package operations

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
	"reflect"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type managerCrashMark struct {
	Stage                                             string
	Identity                                          Identity
	Binding                                           Binding
	Before                                            Operation
	Claims                                            []Claim
	Payload, Started, Result, Request, ResultEnvelope []byte
}

func TestManagerForcedTerminationAtResultBoundaries(t *testing.T) {
	for _, stage := range []string{"result_transaction", "result_committed"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manager.db")
			cmd := exec.Command(os.Args[0], "-test.run=^TestManagerCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DM_MANAGER_CRASH_STAGE="+stage, "DM_MANAGER_CRASH_PATH="+path)
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
				var otherOutput strings.Builder
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "MANAGER_BOUNDARY:") {
						marks <- scanner.Text()
						return
					}
					if otherOutput.Len() < 65536 {
						otherOutput.WriteString(scanner.Text() + "\n")
					}
				}
				marks <- fmt.Sprintf("missing marker: %s; scanner=%v", otherOutput.String(), scanner.Err())
			}()
			var line string
			select {
			case line = <-marks:
			case <-time.After(25 * time.Second):
				t.Fatal("helper did not reach requested result boundary")
			}
			if !strings.HasPrefix(line, "MANAGER_BOUNDARY:") {
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
				t.Fatal("helper exited normally instead of being forcibly terminated")
			}
			markRaw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "MANAGER_BOUNDARY:"))
			if err != nil {
				t.Fatal(err)
			}
			var mark managerCrashMark
			if err := json.Unmarshal(markRaw, &mark); err != nil {
				t.Fatal(err)
			}
			if mark.Stage != stage {
				t.Fatalf("wrong boundary %q", mark.Stage)
			}
			ctx := context.Background()
			s, err := Open(ctx, path, mark.Identity, mfKeys())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			payload, err := s.ReadPayload(ctx, mark.Before.OperationID)
			if err != nil || !bytes.Equal(payload, mark.Payload) {
				t.Fatalf("payload changed across process loss: %v", err)
			}
			started, err := s.ReadEvent(ctx, mark.Before.OperationID, 1)
			if err != nil || !bytes.Equal(started.Body, mark.Started) {
				t.Fatalf("started evidence changed across process loss: %v", err)
			}
			claims, err := s.Claims(ctx, mark.Before.OperationID)
			if err != nil || len(claims) != 2 || !reflect.DeepEqual(claims, mark.Claims) {
				t.Fatalf("claims lost across process loss: %+v %v", claims, err)
			}
			op, err := s.Get(ctx, mark.Before.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			committed := stage == "result_committed"
			if committed {
				result, err := s.ReadEvent(ctx, op.OperationID, 2)
				if err != nil || !bytes.Equal(result.Body, mark.Result) {
					t.Fatalf("committed result body changed: %v", err)
				}
				if op.State != "succeeded" || op.ReleaseStatus != "pending" || op.LatestResultSeq != 2 || op.MaxEventSeq != 2 || op.Revision != mark.Before.Revision+1 {
					t.Fatalf("committed result projection lost: %+v", op)
				}
			} else {
				if _, err := s.ReadEvent(ctx, op.OperationID, 2); !errors.Is(err, ErrNotFound) {
					t.Fatalf("uncommitted result survived: %v", err)
				}
				if !reflect.DeepEqual(op, mark.Before) {
					t.Fatalf("uncommitted result partially changed operation: before=%+v after=%+v", mark.Before, op)
				}
			}
			mfAssertFinalize(t, s, op.OperationID, committed)
			wantEvents := 1
			if committed {
				wantEvents = 2
			}
			if mfCount(t, s, "operation_events") != wantEvents || mfCount(t, s, "event_conflicts") != 0 {
				t.Fatal("partial event or fabricated conflict after process loss")
			}
			// Payload, started, and the result attempt each reserved one wrapping
			// use. Killing before business COMMIT must not refund the third use.
			if mfUses(t, s) != 3 {
				t.Fatal("process loss refunded a reserved KEK use")
			}
			if permit, err := s.AuthorizeStart(ctx, mark.Binding, mark.Request); err == nil || len(permit) != 0 {
				t.Fatalf("Open resurrected old permit: %v", err)
			}
			newBinding := mark.Binding
			newBinding.SessionID = "00000000-0000-4000-8000-000000000031"
			if err := s.BindSessionFixture(ctx, newBinding); err != nil {
				t.Fatal(err)
			}
			if permit, err := s.AuthorizeStart(ctx, newBinding, mfRequest(t, newBinding, op)); err == nil || len(permit) != 0 {
				t.Fatalf("new session resurrected old permit: %v", err)
			}
			// A new connection may replay history despite the authorization
			// freeze. Replacing only the envelope session preserves raw body.
			replay := bytes.Replace(mark.ResultEnvelope, []byte(mark.Binding.SessionID), []byte(newBinding.SessionID), 1)
			ack, err := s.ReceiveEvent(ctx, newBinding, replay)
			if err != nil || len(ack) == 0 {
				t.Fatalf("historical result replay failed: %v", err)
			}
			result, err := s.ReadEvent(ctx, op.OperationID, 2)
			if err != nil || !bytes.Equal(result.Body, mark.Result) {
				t.Fatalf("replayed original result changed: %v", err)
			}
			wantUses := 3
			if !committed {
				wantUses = 4
			}
			if mfUses(t, s) != wantUses {
				t.Fatal("historical replay incorrectly consumed or refunded encryption budget")
			}
			mfAssertFinalize(t, s, op.OperationID, true)
			if claims, err := s.Claims(ctx, op.OperationID); err != nil || !reflect.DeepEqual(claims, mark.Claims) {
				t.Fatalf("ack released claims: %+v %v", claims, err)
			}
			if permit, err := s.AuthorizeStart(ctx, newBinding, mfRequest(t, newBinding, op)); err == nil || len(permit) != 0 {
				t.Fatalf("replayed result resurrected permit: %v", err)
			}
		})
	}
}

func TestManagerCrashHelper(t *testing.T) {
	stage := os.Getenv("DM_MANAGER_CRASH_STAGE")
	if stage == "" {
		return
	}
	path := os.Getenv("DM_MANAGER_CRASH_PATH")
	s, b, prepare := mfFixtureAt(t, path)
	ctx := context.Background()
	op := mfAuthorized(t, s, b, prepare)
	startedRaw := mfEvent(t, b, op, "started")
	if ack, err := s.ReceiveEvent(ctx, b, startedRaw); err != nil || len(ack) == 0 {
		t.Fatalf("started: %v", err)
	}
	op, err := s.Get(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := s.ReadPayload(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := s.Claims(ctx, op.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("initial claims: %+v %v", claims, err)
	}
	resultRaw := mfEvent(t, b, op, "result")
	mark := managerCrashMark{Stage: stage, Identity: s.Identity(), Binding: b, Before: op, Claims: claims, Payload: payload, Started: mfEnvelope(t, startedRaw).Body, Result: mfEnvelope(t, resultRaw).Body, Request: mfRequest(t, b, op), ResultEnvelope: resultRaw}
	boundary := func() {
		raw, err := json.Marshal(mark)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("MANAGER_BOUNDARY:" + base64.StdEncoding.EncodeToString(raw))
		for {
			time.Sleep(time.Hour)
		}
	}
	if stage == "result_transaction" {
		// The actual ReceiveEvent method runs unchanged using the same driver
		// and production connection checks. This TEMP trigger blocks inside
		// the result transaction after event, projection and finalize writes.
		// It vanishes with the killed process and cannot affect the reopened DB.
		if err := s.db.Close(); err != nil {
			t.Fatal(err)
		}
		d := &sqlite.Driver{}
		d.RegisterConnectionHook(verifyConnection)
		if err := d.RegisterFunction("manager_crash_pause", &sqlite.FunctionImpl{NArgs: 0, Scalar: func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { boundary(); return nil, nil }}); err != nil {
			t.Fatal(err)
		}
		sql.Register("manager-crash-sqlite", d)
		s.db, err = sql.Open("manager-crash-sqlite", databaseURI(path))
		if err != nil {
			t.Fatal(err)
		}
		s.db.SetMaxOpenConns(1)
		s.db.SetMaxIdleConns(1)
		if _, err := s.db.Exec(`CREATE TEMP TRIGGER manager_pause AFTER INSERT ON main.outbox WHEN NEW.kind='finalize' BEGIN SELECT manager_crash_pause(); END`); err != nil {
			t.Fatal(err)
		}
	}
	if ack, err := s.ReceiveEvent(ctx, b, resultRaw); err != nil || len(ack) == 0 {
		t.Fatalf("result: %v", err)
	}
	if stage == "result_committed" {
		boundary()
	}
	t.Fatalf("helper did not reach boundary %q", stage)
}
