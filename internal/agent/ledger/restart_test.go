package ledger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/startgate"
)

const crashNode = "00000000-0000-4000-8000-000000000001"

var crashKey = bytes.Repeat([]byte{0x6b}, 32)

// This fixture reuses the wire example instead of introducing a second payload
// shape. None of its values is a real credential or deployment input.
func crashPrepare(t *testing.T) ([]byte, startgate.Request, startgate.Permit) {
	t.Helper()
	raw, err := os.ReadFile("../../../contracts/examples/agent-v1.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	var examples []json.RawMessage
	if err := json.Unmarshal(raw, &examples); err != nil {
		t.Fatal(err)
	}
	for _, example := range examples {
		var message struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Body      struct {
				Operation string `json:"operation_id"`
				Digest    string `json:"input_digest"`
			} `json:"body"`
		}
		if err := json.Unmarshal(example, &message); err != nil {
			t.Fatal(err)
		}
		if message.Type != "prepare" {
			continue
		}
		r := startgate.Request{
			Session: message.SessionID, Operation: message.Body.Operation, Digest: message.Body.Digest,
			Nonce: "00000000-0000-4000-8000-000000000191", MessageID: "00000000-0000-4000-8000-000000000192",
		}
		p := startgate.Permit{
			Session: r.Session, Operation: r.Operation, Digest: r.Digest, Nonce: r.Nonce, ReplyTo: r.MessageID,
			PermitID: "00000000-0000-4000-8000-000000000193", AuthRev: 1,
		}
		return example, r, p
	}
	t.Fatal("prepare example missing")
	return nil, startgate.Request{}, startgate.Permit{}
}

// Each marker is emitted only after the database commit at that boundary has
// returned. The parent forcibly kills the process, without Close or deferred
// cleanup, then checks the actual file database from a fresh Store.
func TestLedgerForcedProcessTermination(t *testing.T) {
	for _, stage := range []string{"before_commit", "after_commit", "after_start"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ledger.db")
			countPath := filepath.Join(dir, "start-count")
			s, err := Create(context.Background(), path, crashNode, "crash-test-key", crashKey)
			if err != nil {
				t.Fatal(err)
			}
			identity := s.Identity()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestLedgerCrashHelper$", "-test.timeout=45s")
			command.Env = append(os.Environ(), "DM_LEDGER_CRASH_STAGE="+stage,
				"DM_LEDGER_CRASH_PATH="+path, "DM_LEDGER_CRASH_COUNT="+countPath,
				"DM_LEDGER_CRASH_IDENTITY="+string(encoded))
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			})
			marker := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "CRASH_BOUNDARY:") {
						marker <- scanner.Text()
						return
					}
				}
				marker <- "missing boundary"
			}()
			select {
			case got := <-marker:
				if got != "CRASH_BOUNDARY:"+stage {
					_ = command.Process.Kill()
					_ = command.Wait()
					waited = true
					t.Fatalf("helper boundary %q; stderr=%s", got, stderr.String())
				}
			case <-time.After(20 * time.Second):
				t.Fatal("helper did not reach the specified boundary")
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err == nil {
				t.Fatal("helper exited normally instead of being killed")
			}
			waited = true
			reopened, err := Open(context.Background(), path, identity, crashKey)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			prepare, request, permit := crashPrepare(t)
			record, err := reopened.Get(context.Background(), request.Operation)
			if err != nil {
				t.Fatal(err)
			}
			wantPhase := PhaseStartCommitted
			if stage == "before_commit" {
				wantPhase = PhaseReady
			}
			if record.Phase != wantPhase || record.Identity != identity {
				t.Fatalf("reopened record phase=%s identity=%+v", record.Phase, record.Identity)
			}
			if stage == "before_commit" {
				if record.Permit != nil || record.StartedAt != "" {
					t.Fatal("start evidence exists before commit")
				}
			} else if record.Permit == nil || *record.Permit != permit || record.StartedAt == "" {
				t.Fatalf("committed start evidence incomplete: %+v", record)
			}
			claims, err := reopened.Claims(context.Background(), request.Operation)
			if err != nil || len(claims) != 2 {
				t.Fatalf("claims lost: %+v %v", claims, err)
			}
			newStarts := 0
			if _, err := reopened.NewGate(context.Background(), request, time.Now, func(startgate.Permit) { newStarts++ }); !errors.Is(err, ErrFrozen) {
				t.Fatalf("old boot gate error=%v, want frozen", err)
			}
			if _, err := reopened.RecordReadyFixture(context.Background(), prepare); !errors.Is(err, ErrFrozen) {
				t.Fatalf("old boot prepare error=%v, want frozen", err)
			}
			if newStarts != 0 {
				t.Fatal("reopen restarted an old operation")
			}
			count, err := os.ReadFile(countPath)
			if stage == "after_start" {
				if err != nil || string(count) != "started\n" {
					t.Fatalf("start count %q, err=%v", count, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("Start was invoked before expected boundary: %q, %v", count, err)
			}
		})
	}
}

func TestLedgerCrashHelper(t *testing.T) {
	stage := os.Getenv("DM_LEDGER_CRASH_STAGE")
	if stage == "" {
		return // Helper is inert during an ordinary package test invocation.
	}
	var identity Identity
	if err := json.Unmarshal([]byte(os.Getenv("DM_LEDGER_CRASH_IDENTITY")), &identity); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), os.Getenv("DM_LEDGER_CRASH_PATH"), identity, crashKey)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately no Close: the parent must test process termination recovery.
	prepare, request, permit := crashPrepare(t)
	if _, err := s.RecordReadyFixture(context.Background(), prepare); err != nil {
		t.Fatal(err)
	}
	boundary := func() {
		fmt.Println("CRASH_BOUNDARY:" + stage)
		for {
			time.Sleep(time.Hour)
		}
	}
	start := func(startgate.Permit) {
		f, err := os.OpenFile(os.Getenv("DM_LEDGER_CRASH_COUNT"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("started\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		boundary()
	}
	if stage == "before_commit" {
		boundary()
	}
	if stage == "after_commit" {
		// Wrap the real commit boundary to pause before Gate can call Start.
		g, err := startgate.New(request, startgate.Hooks{Now: time.Now, Start: start,
			CommitStart: func(p startgate.Permit) error {
				if err := s.commitStart(context.Background(), p); err != nil {
					return err
				}
				boundary()
				return nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Authorize(permit); err != nil {
			t.Fatal(err)
		}
	} else if stage == "after_start" {
		g, err := s.NewGate(context.Background(), request, time.Now, start)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Authorize(permit); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatalf("unknown helper stage %q", stage)
	}
	t.Fatal("helper returned without reaching its boundary")
}
