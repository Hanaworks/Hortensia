package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/startgate"
)

func TestJournalEventStatementAndCommitFailuresAreAtomic(t *testing.T) {
	for _, kind := range []string{"started", "result"} {
		for _, commitFailure := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: " statement", true: " commit"}[commitFailure], func(t *testing.T) {
				s, _, _, _ := eventTestStore(t)
				ctx := context.Background()
				r, q, p := gateReady(t, s)
				starts := 0
				g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) { starts++ })
				if err != nil {
					t.Fatal(err)
				}
				if commitFailure {
					_, err = s.db.Exec(`CREATE TABLE event_fault(parent TEXT REFERENCES executions(operation_id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_event AFTER INSERT ON execution_events BEGIN INSERT INTO event_fault(parent) VALUES('private-event-secret'); END`)
				} else {
					_, err = s.db.Exec(`CREATE TRIGGER fail_event BEFORE INSERT ON execution_events BEGIN SELECT RAISE(ABORT,'private-event-secret'); END`)
				}
				if err != nil {
					t.Fatal(err)
				}
				if kind == "started" {
					err = g.Authorize(p)
				} else {
					_, err = s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1))
				}
				if !errors.Is(err, ErrStorage) || strings.Contains(err.Error(), "private-event-secret") {
					t.Fatalf("failure: %v", err)
				}
				if starts != 0 {
					t.Fatal("start escaped failed transaction")
				}
				gateAssertFacts(t, s, r.OperationID, PhaseReady, nil)
				var count, last, uses int
				var latest any
				s.db.QueryRow("SELECT count(*) FROM execution_events").Scan(&count)
				s.db.QueryRow("SELECT last_seq,latest_result_seq FROM event_state").Scan(&last, &latest)
				s.db.QueryRow("SELECT uses FROM event_keys").Scan(&uses)
				if count != 0 || last != 0 || latest != nil || uses != 1 {
					t.Fatalf("partial write or returned budget: count=%d last=%d latest=%v uses=%d", count, last, latest, uses)
				}
			})
		}
	}
}

func TestJournalCancelAfterCommitPreservesStarted(t *testing.T) {
	s, _, _, _ := eventTestStore(t)
	_, q, p := gateReady(t, s)
	committed := make(chan struct{})
	release := make(chan struct{})
	starts := 0
	g := gateWithBarrier(t, s, q, time.Now, func(p startgate.Permit) error {
		err := s.commitStart(context.Background(), p)
		close(committed)
		<-release
		return err
	}, func(startgate.Permit) { starts++ })
	done := make(chan error, 1)
	go func() { done <- g.Authorize(p) }()
	gateAwait(t, committed)
	g.Cancel()
	close(release)
	if err := gateResult(t, done); err == nil {
		t.Fatal("canceled gate reported start")
	}
	if starts != 0 {
		t.Fatal("canceled gate started")
	}
	e, err := s.ReadEvent(context.Background(), p.Operation, 1)
	if err != nil || e.Kind != "started" {
		t.Fatalf("cancellation erased evidence: %v", err)
	}
	gateAssertFacts(t, s, p.Operation, PhaseStartCommitted, &p)
}

func TestJournalPrepareStateFailureLeavesNoRecordOrClaims(t *testing.T) {
	s, _, _, _ := eventTestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_state BEFORE INSERT ON event_state BEGIN SELECT RAISE(ABORT,'state insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t))); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	for _, table := range []string{"executions", "local_claims", "event_state"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial prepare %s: %d %v", table, count, err)
		}
	}
}

func TestJournalGateRechecksBlockedAndResultAfterConstruction(t *testing.T) {
	for _, change := range []string{"blocked", "result"} {
		t.Run(change, func(t *testing.T) {
			s, _, _, _ := eventTestStore(t)
			ctx := context.Background()
			r, q, p := gateReady(t, s)
			starts := 0
			g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) { starts++ })
			if err != nil {
				t.Fatal(err)
			}
			if change == "blocked" {
				if _, err := s.db.Exec("UPDATE event_state SET blocked=1 WHERE operation_id=?", r.OperationID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.Authorize(p); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed state allowed start: %v", err)
			}
			if starts != 0 {
				t.Fatal("started after state changed")
			}
			gateAssertFacts(t, s, r.OperationID, PhaseReady, nil)
		})
	}
}

func TestJournalCanceledResultAndAckPreserveCommittedState(t *testing.T) {
	s, _, _, _ := eventTestStore(t)
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	raw := resultFixtureFor(t, r, "not_started", 1)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.RecordResultFixture(canceled, raw); !errors.Is(err, ErrStorage) {
		t.Fatalf("canceled result: %v", err)
	}
	var count, uses int
	if err := s.db.QueryRow("SELECT count(*) FROM execution_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT uses FROM event_keys").Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if count != 0 || uses != 0 {
		t.Fatal("canceled result published evidence or used quota")
	}
	if _, err := s.RecordResultFixture(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeEvent(canceled, r.OperationID, r.Digest, 1, r.PreparedSession); !errors.Is(err, ErrStorage) {
		t.Fatalf("canceled ack: %v", err)
	}
	e, err := s.ReadEvent(ctx, r.OperationID, 1)
	if err != nil || e.AckSession != "" {
		t.Fatalf("canceled ack persisted: %v", err)
	}
}
