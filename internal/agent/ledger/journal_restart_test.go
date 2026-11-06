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

type journalCrashMark struct {
	Stage           string
	Started, Result []byte
}

func TestJournalForcedTerminationAtEventBoundaries(t *testing.T) {
	for _, stage := range []string{"started", "result_transaction", "result", "ack"} {
		t.Run(stage, func(t *testing.T) {
			s, path := recoveryFixtureStore(t)
			identity := s.Identity()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			encodedID, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			counter := filepath.Join(filepath.Dir(path), "start-counter")
			cmd := exec.Command(os.Args[0], "-test.run=^TestJournalCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DM_JOURNAL_STAGE="+stage, "DM_JOURNAL_PATH="+path,
				"DM_JOURNAL_IDENTITY="+string(encodedID), "DM_JOURNAL_COUNTER="+counter)
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
				var helperOutput strings.Builder
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "JOURNAL_BOUNDARY:") {
						marks <- scanner.Text()
						return
					}
					if helperOutput.Len() < 65536 {
						helperOutput.WriteString(scanner.Text() + "\n")
					}
				}
				marks <- "missing marker: " + helperOutput.String()
			}()
			var line string
			select {
			case line = <-marks:
			case <-time.After(20 * time.Second):
				t.Fatal("helper did not reach requested boundary")
			}
			if !strings.HasPrefix(line, "JOURNAL_BOUNDARY:") {
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
				t.Fatal("helper was not forcibly terminated")
			}
			markerBytes, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "JOURNAL_BOUNDARY:"))
			if err != nil {
				t.Fatal(err)
			}
			var mark journalCrashMark
			if err := json.Unmarshal(markerBytes, &mark); err != nil {
				t.Fatal(err)
			}
			if mark.Stage != stage {
				t.Fatalf("wrong boundary %q", mark.Stage)
			}
			reopened, err := OpenJournal(context.Background(), path, identity, crashKey, recoveryKEK)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			_, q, _ := crashPrepare(t)
			started, err := reopened.ReadEvent(context.Background(), q.Operation, 1)
			if err != nil || !bytes.Equal(started.Body, mark.Started) {
				t.Fatalf("started body changed: %v", err)
			}
			view := recoveryDecode(t, reopened, q.Operation)
			wantSeq, wantUses := uint64(1), int64(1)
			if stage != "started" {
				wantUses = 2
			}
			if stage == "result" || stage == "ack" {
				wantSeq = 2
				result, err := reopened.ReadEvent(context.Background(), q.Operation, 2)
				if err != nil || !bytes.Equal(result.Body, mark.Result) {
					t.Fatalf("result body changed: %v", err)
				}
				wantAck := ""
				if stage == "ack" {
					wantAck = q.Session
				}
				if result.AckSession != wantAck || view["report_acked"] != (stage == "ack") {
					t.Fatalf("ack not durable: %+v %+v", result, view)
				}
				if view["phase"] != "result" {
					t.Fatalf("result projection: %+v", view)
				}
			} else {
				if _, err := reopened.ReadEvent(context.Background(), q.Operation, 2); !errors.Is(err, ErrNotFound) {
					t.Fatalf("uncommitted result survived: %v", err)
				}
				if view["phase"] != "reconciliation_required" || view["latest_result"] != nil {
					t.Fatalf("fabricated outcome: %+v", view)
				}
			}
			if view["last_event_seq"] != float64(wantSeq) {
				t.Fatalf("sequence=%v", view["last_event_seq"])
			}
			var uses int64
			if err := reopened.db.QueryRow("SELECT uses FROM event_keys WHERE id=1").Scan(&uses); err != nil || uses != wantUses {
				t.Fatalf("reserved uses=%d want%d: %v", uses, wantUses, err)
			}
			pending, err := reopened.PendingEvents(context.Background(), q.Operation, gateOtherID)
			if err != nil || len(pending) != int(wantSeq) {
				t.Fatalf("new-session replay count=%d err=%v", len(pending), err)
			}
			if _, err := reopened.NewGate(context.Background(), q, time.Now, func(startgate.Permit) { t.Error("restarted old operation") }); !errors.Is(err, ErrFrozen) {
				t.Fatalf("old gate: %v", err)
			}
			count, err := os.ReadFile(counter)
			if err != nil || string(count) != "started\n" {
				t.Fatalf("unexpected start count %q %v", count, err)
			}
		})
	}
}

func TestJournalCrashHelper(t *testing.T) {
	stage := os.Getenv("DM_JOURNAL_STAGE")
	if stage == "" {
		return
	}
	var identity Identity
	if err := json.Unmarshal([]byte(os.Getenv("DM_JOURNAL_IDENTITY")), &identity); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("DM_JOURNAL_PATH")
	s, err := OpenJournal(context.Background(), path, identity, crashKey, recoveryKEK)
	if err != nil {
		t.Fatal(err)
	}
	// Intentionally no Close: the parent must observe abnormal process loss.
	prepare, q, p := crashPrepare(t)
	r, err := s.RecordReadyFixture(context.Background(), prepare)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) {
		f, err := os.OpenFile(os.Getenv("DM_JOURNAL_COUNTER"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(p); err != nil {
		t.Fatal(err)
	}
	started, err := s.ReadEvent(context.Background(), q.Operation, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw := recoveryResultRaw(t, r, 2, "normal_exit")
	var envelope protocol.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	mark := journalCrashMark{stage, started.Body, envelope.Body}
	boundary := func() {
		encoded, err := json.Marshal(mark)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("JOURNAL_BOUNDARY:" + base64.StdEncoding.EncodeToString(encoded))
		for {
			time.Sleep(time.Hour)
		}
	}
	if stage == "started" {
		boundary()
	}
	if stage == "result_transaction" {
		// Use the identical SQLite driver/settings plus a test-only scalar
		// barrier. The actual RecordResultFixture transaction runs unchanged;
		// a TEMP trigger pauses after both result insert and pointer update,
		// before COMMIT. TEMP objects disappear when this process is killed.
		if err := s.db.Close(); err != nil {
			t.Fatal(err)
		}
		d := &sqlite.Driver{}
		d.RegisterConnectionHook(verifyConnection)
		if err := d.RegisterFunction("journal_crash_pause", &sqlite.FunctionImpl{NArgs: 0, Scalar: func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { boundary(); return nil, nil }}); err != nil {
			t.Fatal(err)
		}
		sql.Register("journal-crash-sqlite", d)
		s.db, err = sql.Open("journal-crash-sqlite", databaseURI(path))
		if err != nil {
			t.Fatal(err)
		}
		s.db.SetMaxOpenConns(1)
		s.db.SetMaxIdleConns(1)
		if _, err := s.db.Exec(`CREATE TEMP TRIGGER journal_pause AFTER UPDATE OF latest_result_seq ON main.event_state WHEN NEW.latest_result_seq IS NOT NULL BEGIN SELECT journal_crash_pause(); END`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordResultFixture(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if stage == "result" {
		boundary()
	}
	if stage == "ack" {
		if err := s.AcknowledgeEvent(context.Background(), q.Operation, q.Digest, 2, q.Session); err != nil {
			t.Fatal(err)
		}
		boundary()
	}
	t.Fatalf("helper did not reach boundary %q", stage)
}
