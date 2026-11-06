package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/startgate"
)

func eventTestStore(t *testing.T) (*Store, string, []byte, JournalKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.sqlite")
	key := bytes.Repeat([]byte{0x42}, 32)
	jk := JournalKey{ID: "events-1", Key: bytes.Repeat([]byte{0x73}, 32)}
	s, err := CreateJournal(context.Background(), path, testNode, "key-1", key, jk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, path, key, jk
}

func TestJournalStartedVisibleInsideStartAndAckPreservesEvidence(t *testing.T) {
	s, _, _, _ := eventTestStore(t)
	ctx := context.Background()
	r, q, p := gateReady(t, s)
	starts := 0
	g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {
		starts++
		e, err := s.ReadEvent(ctx, r.OperationID, 1)
		if err != nil {
			t.Error(err)
			return
		}
		stored, err := s.Get(ctx, r.OperationID)
		if err != nil {
			t.Error(err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(e.Body, &body); err != nil {
			t.Error(err)
		}
		if e.Kind != "started" || e.Seq != 1 || body["started_at"] != stored.StartedAt || body["permit_id"] != p.PermitID || body["phase"] != "START_COMMITTED" {
			t.Errorf("incomplete started: %+v %+v", e, body)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(p); err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(p); err != nil || starts != 1 {
		t.Fatalf("duplicate starts=%d err=%v", starts, err)
	}
	e, err := s.ReadEvent(ctx, r.OperationID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, r.PreparedSession); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, r.PreparedSession); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingEvents(ctx, r.OperationID, r.PreparedSession)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acked pending: %+v %v", pending, err)
	}
	pending, err = s.PendingEvents(ctx, r.OperationID, gateOtherID)
	if err != nil || len(pending) != 1 || !bytes.Equal(pending[0].Body, e.Body) || pending[0].AckSession != r.PreparedSession {
		t.Fatalf("new session pending: %+v %v", pending, err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", 1, gateOtherID); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong digest: %v", err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 2, gateOtherID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong seq: %v", err)
	}
	gateAssertFacts(t, s, r.OperationID, PhaseStartCommitted, &p)
}

func TestJournalReadRejectsAuthenticatedInvalidOrMismatchedBody(t *testing.T) {
	for _, name := range []string{"schema", "operation", "digest", "sequence", "kind"} {
		t.Run(name, func(t *testing.T) {
			s, _, _, _ := eventTestStore(t)
			ctx := context.Background()
			r, _, p := gateReady(t, s)
			if err := s.commitStart(ctx, p); err != nil {
				t.Fatal(err)
			}
			e, err := s.ReadEvent(ctx, r.OperationID, 1)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			json.Unmarshal(e.Body, &body)
			switch name {
			case "schema":
				delete(body, "started_at")
			case "operation":
				body["operation_id"] = gateOtherID
			case "digest":
				body["input_digest"] = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
			case "sequence":
				body["event_seq"] = 2
			case "kind":
				body["phase"] = "result"
			}
			sealed, err := s.sealEvent(ctx, eventBinding{OperationID: r.OperationID, Digest: r.Digest, Kind: "started", Seq: 1}, encodeFixture(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE execution_events SET body_cipher=?,wrapped_dek=?,body_fingerprint=?", sealed.Cipher, sealed.WrappedDEK, sealed.Fingerprint); err != nil {
				t.Fatal(err)
			}
			if e, err := s.ReadEvent(ctx, r.OperationID, 1); !errors.Is(err, ErrEvidence) || e.Body != nil {
				t.Fatalf("invalid evidence exposed: %v", err)
			}
			if events, err := s.PendingEvents(ctx, r.OperationID, r.PreparedSession); !errors.Is(err, ErrEvidence) || events != nil {
				t.Fatalf("partial pending evidence: %v", err)
			}
		})
	}
}

func TestJournalGateRegistrationDoesNotDeadlockTransactionalDecrypt(t *testing.T) {
	s, _, _, _ := eventTestStore(t)
	ctx := context.Background()
	r, q, p := gateReady(t, s)
	if err := s.commitStart(ctx, p); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	before := s.db.Stats().WaitCount
	gateDone := make(chan error, 1)
	go func() { _, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {}); gateDone <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for s.db.Stats().WaitCount == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.db.Stats().WaitCount == before {
		t.Fatal("gate did not wait for connection")
	}
	readDone := make(chan error, 1)
	go func() { _, err := s.readEvent(ctx, tx, r.OperationID, 1); readDone <- err }()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		tx.Rollback()
		<-gateDone
		<-readDone
		t.Fatal("Gate lock blocked transactional decryption")
	}
	tx.Rollback()
	if err := <-gateDone; !errors.Is(err, ErrAlreadyCommitted) {
		t.Fatal(err)
	}
}

func TestJournalReadDoesNotExpandOriginalBodyDuringValidation(t *testing.T) {
	s, _, _, _ := eventTestStore(t)
	ctx := context.Background()
	r, _, p := gateReady(t, s)
	if err := s.commitStart(ctx, p); err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	json.Unmarshal(resultFixtureFor(t, r, "normal_exit", 2), &msg)
	items := make([]any, 2000)
	for i := range items {
		items[i] = map[string]any{"resource_kind": "container", "resource_id": strings.Repeat("a", 64), "name": strings.Repeat("<", 256), "project_name": strings.Repeat("<", 256), "service_name": strings.Repeat("<", 256), "stack_id": nil, "image_id": nil, "state": "running", "health": "none"}
	}
	msg["body"].(map[string]any)["observation"].(map[string]any)["items"] = items
	var wire bytes.Buffer
	encoder := json.NewEncoder(&wire)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(msg); err != nil {
		t.Fatal(err)
	}
	first, err := s.RecordResultFixture(ctx, wire.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.ReadEvent(ctx, r.OperationID, 2)
	if err != nil || !bytes.Equal(read.Body, first.Body) {
		t.Fatalf("valid original body expanded or rejected: %v", err)
	}
}

func TestJournalAPIsRejectVersionOneStore(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	r, _, _ := gateReady(t, s)
	if _, err := s.ReadEvent(ctx, r.OperationID, 1); !errors.Is(err, ErrJournal) {
		t.Fatal(err)
	}
	if _, err := s.PendingEvents(ctx, r.OperationID, r.PreparedSession); !errors.Is(err, ErrJournal) {
		t.Fatal(err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, r.PreparedSession); !errors.Is(err, ErrJournal) {
		t.Fatal(err)
	}
	if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); !errors.Is(err, ErrJournal) {
		t.Fatal(err)
	}
}
