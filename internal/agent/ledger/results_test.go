package ledger

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

func resultFixtureFor(t *testing.T, r Record, outcome string, seq uint64) []byte {
	t.Helper()
	body := map[string]any{"operation_id": r.OperationID, "input_digest": r.Digest, "event_seq": seq, "phase": "result", "outcome": outcome, "interrupt_reason": nil, "exit_code": nil, "error": nil, "observation": nil, "not_started": outcome == "not_started", "output_b64": nil, "output_truncated": false, "finished_at": "2026-11-04T04:00:00.000Z", "ownership_status": "unknown"}
	if outcome == "normal_exit" {
		body["exit_code"] = 0
		body["ownership_status"] = "consistent"
		body["observation"] = map[string]any{"status": "complete", "collected_at": "2026-11-04T04:00:00.000Z", "items": []any{}, "error": nil}
	} else {
		body["error"] = map[string]any{"category": "execution_failed", "code": "fixture_result", "message": "controlled fixture", "retry_action": "query_operation"}
	}
	if outcome == "not_started" {
		body["ownership_status"] = "not_checked"
	}
	if outcome == "interrupted" {
		body["interrupt_reason"] = "canceled"
	}
	raw, err := protocol.NewMessage("result", r.PreparedSession, "", body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestJournalFirstResultsAndDuplicateConflict(t *testing.T) {
	for _, outcome := range []string{"not_started", "normal_exit", "interrupted", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			s, path, key, jk := eventTestStore(t)
			ctx := context.Background()
			r, q, p := gateReady(t, s)
			seq := uint64(1)
			if outcome != "not_started" {
				if err := s.commitStart(ctx, p); err != nil {
					t.Fatal(err)
				}
				seq = 2
			}
			raw := resultFixtureFor(t, r, outcome, seq)
			env, err := s.validator.Validate(raw)
			if err != nil {
				t.Fatal(err)
			}
			first, err := s.RecordResultFixture(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Body, env.Body) || first.Seq != seq {
				t.Fatal("result bytes or sequence changed")
			}
			var uses int
			if err := s.db.QueryRow("SELECT uses FROM event_keys").Scan(&uses); err != nil {
				t.Fatal(err)
			}
			again, err := s.RecordResultFixture(ctx, raw)
			if err != nil || !bytes.Equal(again.Body, first.Body) {
				t.Fatalf("duplicate: %v", err)
			}
			var after int
			s.db.QueryRow("SELECT uses FROM event_keys").Scan(&after)
			if after != uses {
				t.Fatal("sequential duplicate consumed budget")
			}
			if _, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {}); err == nil {
				t.Fatal("result granted a gate")
			}
			future := resultFixtureFor(t, r, outcome, seq+1)
			if _, err := s.RecordResultFixture(ctx, future); !errors.Is(err, ErrConflict) {
				t.Fatalf("future version: %v", err)
			}
			identity := s.Identity()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenJournal(ctx, path, identity, key, jk)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			again, err = reopened.RecordResultFixture(ctx, raw)
			if err != nil || !bytes.Equal(again.Body, first.Body) {
				t.Fatalf("old boot exact replay: %v", err)
			}
			changed := bytes.Replace(raw, []byte("2026-11-04T04:00:00.000Z"), []byte("2026-11-04T04:00:01.000Z"), 1)
			if _, err := reopened.RecordResultFixture(ctx, changed); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflict: %v", err)
			}
			e, err := reopened.ReadEvent(ctx, r.OperationID, seq)
			if err != nil || !bytes.Equal(e.Body, first.Body) {
				t.Fatalf("conflict destroyed original: %v", err)
			}
			var blocked, count, last, latest int
			reopened.db.QueryRow("SELECT blocked,last_seq,latest_result_seq FROM event_state WHERE operation_id=?", r.OperationID).Scan(&blocked, &last, &latest)
			reopened.db.QueryRow("SELECT count(*) FROM event_conflicts").Scan(&count)
			if blocked != 1 || count != 1 || last != int(seq) || latest != int(seq) {
				t.Fatalf("conflict state: %d %d %d %d", blocked, count, last, latest)
			}
			claims, err := reopened.Claims(ctx, r.OperationID)
			if err != nil || len(claims) != 2 {
				t.Fatal("claims released")
			}
		})
	}
}

func TestJournalResultRejectsWrongBindingAndState(t *testing.T) {
	for _, name := range []string{"digest", "operation", "session", "sequence", "outcome", "old boot", "claims"} {
		t.Run(name, func(t *testing.T) {
			s, path, key, jk := eventTestStore(t)
			ctx := context.Background()
			r, _, _ := gateReady(t, s)
			raw := resultFixtureFor(t, r, "not_started", 1)
			var msg map[string]any
			json.Unmarshal(raw, &msg)
			body := msg["body"].(map[string]any)
			switch name {
			case "digest":
				body["input_digest"] = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
			case "operation":
				body["operation_id"] = gateOtherID
			case "session":
				msg["session_id"] = gateOtherID
			case "sequence":
				body["event_seq"] = 2
			case "outcome":
				raw = resultFixtureFor(t, r, "unknown", 1)
			case "claims":
				s.db.Exec("DELETE FROM local_claims WHERE operation_id=? AND resource_kind='stack'", r.OperationID)
			case "old boot":
				id := s.Identity()
				s.Close()
				var err error
				s, err = OpenJournal(ctx, path, id, key, jk)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
			}
			if name != "outcome" {
				raw = encodeFixture(t, msg)
			}
			if _, err := s.RecordResultFixture(ctx, raw); err == nil {
				t.Fatal("invalid result accepted")
			}
			var count int
			s.db.QueryRow("SELECT count(*) FROM execution_events").Scan(&count)
			if count != 0 {
				t.Fatal("invalid result left event")
			}
		})
	}
}

func TestJournalNotStartedRacesStart(t *testing.T) {
	for i := 0; i < 10; i++ {
		s, _, _, _ := eventTestStore(t)
		ctx := context.Background()
		r, _, p := gateReady(t, s)
		raw := resultFixtureFor(t, r, "not_started", 1)
		done := make(chan error, 2)
		go func() { done <- s.commitStart(ctx, p) }()
		go func() { _, err := s.RecordResultFixture(ctx, raw); done <- err }()
		a, b := <-done, <-done
		if (a == nil) == (b == nil) {
			t.Fatalf("expected one winner: %v %v", a, b)
		}
		e, err := s.ReadEvent(ctx, r.OperationID, 1)
		if err != nil {
			t.Fatal(err)
		}
		record, err := s.Get(ctx, r.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if (e.Kind == "started") != (record.Phase == PhaseStartCommitted) {
			t.Fatal("mixed start and result")
		}
		var count int
		s.db.QueryRow("SELECT count(*) FROM execution_events").Scan(&count)
		if count != 1 {
			t.Fatal("race persisted multiple events")
		}
	}
}

func TestJournalResultExactBytesNumericSequenceAndBudgets(t *testing.T) {
	for _, number := range []string{"1.0", "1e0"} {
		t.Run(number, func(t *testing.T) {
			s, _, _, _ := eventTestStore(t)
			ctx := context.Background()
			r, _, _ := gateReady(t, s)
			raw := bytes.Replace(resultFixtureFor(t, r, "not_started", 1), []byte(`"event_seq":1`), []byte(`"event_seq":`+number), 1)
			raw = bytes.Replace(raw, []byte(`"body":{`), []byte("\"body\":{ \n "), 1)
			env, err := s.validator.Validate(raw)
			if err != nil {
				t.Fatal(err)
			}
			e, err := s.RecordResultFixture(ctx, raw)
			if err != nil || !bytes.Equal(e.Body, env.Body) || e.Seq != 1 {
				t.Fatalf("numeric exact bytes: %v", err)
			}
			read, err := s.ReadEvent(ctx, r.OperationID, 1)
			if err != nil || !bytes.Equal(read.Body, env.Body) {
				t.Fatalf("read re-encoded body: %v", err)
			}
			canonical := resultFixtureFor(t, r, "not_started", 1)
			if _, err := s.RecordResultFixture(ctx, canonical); !errors.Is(err, ErrConflict) {
				t.Fatalf("different bytes were deduplicated: %v", err)
			}
		})
	}
	for _, size := range []int{(7 << 20), (7 << 20) + 1} {
		t.Run(fmt.Sprint("body-", size), func(t *testing.T) {
			s, _, _, _ := eventTestStore(t)
			ctx := context.Background()
			r, _, _ := gateReady(t, s)
			raw := resultFixtureFor(t, r, "not_started", 1)
			var env protocol.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			body := append([]byte{'{'}, bytes.Repeat([]byte{' '}, size-len(env.Body))...)
			body = append(body, env.Body[1:]...)
			raw = bytes.Replace(raw, env.Body, body, 1)
			e, err := s.RecordResultFixture(ctx, raw)
			if size == 7<<20 {
				if err != nil || !bytes.Equal(e.Body, body) {
					t.Fatalf("body boundary: %v", err)
				}
			} else if !errors.Is(err, ErrInput) {
				t.Fatalf("oversize body: %v", err)
			}
		})
	}
	for _, size := range []int{1 << 20, (1 << 20) + 1} {
		t.Run(fmt.Sprint("output-", size), func(t *testing.T) {
			s, _, _, _ := eventTestStore(t)
			ctx := context.Background()
			r, _, p := gateReady(t, s)
			if err := s.commitStart(ctx, p); err != nil {
				t.Fatal(err)
			}
			raw := resultFixtureFor(t, r, "unknown", 2)
			var msg map[string]any
			json.Unmarshal(raw, &msg)
			msg["body"].(map[string]any)["output_b64"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, size))
			_, err := s.RecordResultFixture(ctx, encodeFixture(t, msg))
			if size == 1<<20 {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrInput) {
				t.Fatalf("oversize output: %v", err)
			}
		})
	}
}

func TestJournalConcurrentResultDuplicateAndExhaustedReplay(t *testing.T) {
	s, _, _, _ := eventTestStore(t)
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	raw := resultFixtureFor(t, r, "not_started", 1)
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { _, err := s.RecordResultFixture(ctx, raw); done <- err }()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var count, last, latest int
	s.db.QueryRow("SELECT count(*) FROM execution_events").Scan(&count)
	s.db.QueryRow("SELECT last_seq,latest_result_seq FROM event_state").Scan(&last, &latest)
	if count != 1 || last != 1 || latest != 1 {
		t.Fatal("duplicate allocated another event")
	}
	if _, err := s.db.Exec("UPDATE event_keys SET uses=1048576"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordResultFixture(ctx, raw); err != nil {
		t.Fatalf("exhausted replay: %v", err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, r.PreparedSession); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadEvent(ctx, r.OperationID, 1); err != nil {
		t.Fatal(err)
	}
}
