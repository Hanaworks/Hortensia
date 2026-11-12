package ledger

import (
	"bytes"
	"context"
	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeReviewUnfinalizedCandidateCannotBecomeActive(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	runtimeStart(t, s, r, nil)
	if err := s.RecordProcessCreated(ctx, r.OperationID, 1234); err != nil {
		t.Fatal(err)
	}
	zero := 0
	fact := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli()), Output: []byte("review")}
	ev, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, runtimeObservation(t, r, "complete"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE project_bindings SET status='active',ownership_revision=1,last_operation_id=?,baseline_seq=? WHERE endpoint_id=? AND project_name=?`, r.OperationID, ev.Seq, r.EndpointID, r.ProjectName); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadProjectBinding(ctx, r.EndpointID, r.ProjectName)
	if !errors.Is(err, ErrEvidence) {
		t.Fatalf("unfinalized candidate accepted as active: %#v err=%v", got, err)
	}
}
func TestRuntimeReviewWrongDigestCancelMustNotChangeGate(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	r := runtimePrepare(t, s)
	if _, err := s.CommitPrepared(ctx, r.OperationID, runtimeEvidence(t, s, r)); err != nil {
		t.Fatal(err)
	}
	q := startgate.Request{Session: rootScanSession, Operation: r.OperationID, Digest: r.Digest, Nonce: scanContractID(811), MessageID: scanContractID(812)}
	starts := 0
	g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) { starts++ })
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := protocol.NewMessage("cancel", rootScanSession, "", map[string]any{"operation_id": r.OperationID, "input_digest": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{91}, 32))})
	if _, err = s.CancelExecution(ctx, raw); !errors.Is(err, ErrConflict) {
		t.Fatalf("bad digest: %v", err)
	}
	if g.State() != startgate.Waiting {
		t.Fatalf("rejected cancel changed gate to %v", g.State())
	}
	state, err := readEventState(ctx, s.db, r.OperationID)
	if err != nil || state.LastSeq != 0 {
		t.Fatalf("rejected cancel changed events: %+v %v", state, err)
	}
	permit := startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce, PermitID: scanContractID(813), ReplyTo: q.MessageID, AuthRev: 1}
	if err = g.Authorize(permit); err != nil {
		t.Fatalf("legal permit rejected after bad cancel: %v", err)
	}
	if err = g.Authorize(permit); err != nil || starts != 1 {
		t.Fatalf("legal start count=%d error=%v", starts, err)
	}
}

func runtimeReviewCandidate(t *testing.T, s *Store, operation string, collect ...func(Record) ObservationEvidence) (Record, Event) {
	t.Helper()
	ctx := context.Background()
	var r Record
	if operation == "" {
		r = runtimePrepare(t, s)
	} else {
		m := prepareFixture(t)
		m["session_id"] = rootScanSession
		m["body"].(map[string]any)["operation_id"] = operation
		receipt, err := s.ReceivePrepare(ctx, encodeFixture(t, m))
		if err != nil {
			t.Fatal(err)
		}
		r = receipt.Record
	}
	prep := runtimeEvidence(t, s, r)
	binding, err := s.ReadProjectBinding(ctx, r.EndpointID, r.ProjectName)
	if err == nil {
		prep.OwnershipRevision = binding.Revision
		prep.Pre = binding.Baseline
		prep.Pre.CollectedAt = timestamp(time.Now().UnixMilli())
	} else if err != ErrNotFound {
		t.Fatal(err)
	}
	if _, err = s.CommitPrepared(ctx, r.OperationID, prep); err != nil {
		t.Fatal(err)
	}
	nonce, _ := protocol.NewID()
	message, _ := protocol.NewID()
	permitID, _ := protocol.NewID()
	q := startgate.Request{Session: rootScanSession, Operation: r.OperationID, Digest: r.Digest, Nonce: nonce, MessageID: message}
	gate, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {})
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.Authorize(startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce, PermitID: permitID, ReplyTo: q.MessageID, AuthRev: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordProcessCreated(ctx, r.OperationID, 65); err != nil {
		t.Fatal(err)
	}
	exit := 0
	fact := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &exit, FinishedAt: timestamp(time.Now().UnixMilli())}
	observation := runtimeObservation(t, r, "complete")
	if len(collect) > 0 {
		observation = collect[0](r)
	}
	event, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, observation)
	if err != nil {
		t.Fatal(err)
	}
	return r, event
}
func TestRuntimeReviewBindingReferentsAndStatusAreAuthenticated(t *testing.T) {
	cases := []struct {
		name     string
		finalize bool
		mutate   func(*testing.T, *Store, Record, Event)
	}{
		{"active_without_pointer", false, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`UPDATE project_bindings SET status='active'`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"provisional_with_pointer", true, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`UPDATE project_bindings SET status='provisional'`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong_revision", true, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`UPDATE project_bindings SET ownership_revision=ownership_revision+1`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong_result_pointer", true, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`UPDATE project_bindings SET baseline_seq=1`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"missing_receipt", true, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`DELETE FROM finalizations`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"damaged_receipt", true, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`UPDATE finalizations SET body_fingerprint=zeroblob(32)`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"claims_restored", true, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`INSERT INTO local_claims(resource_kind,resource_id,operation_id) VALUES('stack',?,?)`, r.StackID, r.OperationID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong_stack", true, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`UPDATE project_bindings SET stack_id=?`, scanContractID(840))
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"invented_drift_status", true, func(t *testing.T, s *Store, r Record, e Event) {
			_, err := s.db.Exec(`UPDATE project_bindings SET status='drifted'`)
			if err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _ := runtimeStore(t)
			r, event := runtimeReviewCandidate(t, s, "")
			if tc.finalize {
				if _, err := s.Finalize(context.Background(), releaseRequest(t, r, event.Seq, "succeeded")); err != nil {
					t.Fatal(err)
				}
			}
			tc.mutate(t, s, r, event)
			if b, err := s.ReadProjectBinding(context.Background(), r.EndpointID, r.ProjectName); !errors.Is(err, ErrEvidence) {
				t.Fatalf("damaged binding accepted: %+v %v", b, err)
			}
		})
	}
}
func TestRuntimeReviewPublishedPointerCannotRollBackToOlderCandidate(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	first, firstEvent := runtimeReviewCandidate(t, s, "")
	if _, err := s.Finalize(ctx, releaseRequest(t, first, firstEvent.Seq, "succeeded")); err != nil {
		t.Fatal(err)
	}
	next, nextEvent := runtimeReviewCandidate(t, s, scanContractID(850))
	if _, err := s.Finalize(ctx, releaseRequest(t, next, nextEvent.Seq, "succeeded")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE project_bindings SET last_operation_id=?,baseline_seq=?,ownership_revision=1`, first.OperationID, firstEvent.Seq); err != nil {
		t.Fatal(err)
	}
	if b, err := s.ReadProjectBinding(ctx, first.EndpointID, first.ProjectName); !errors.Is(err, ErrEvidence) {
		t.Fatalf("old authenticated candidate replaced newer publication: %+v %v", b, err)
	}
}

func runtimeReviewGate(t *testing.T, s *Store, start func()) (Record, *startgate.Gate, startgate.Permit) {
	t.Helper()
	ctx := context.Background()
	r := runtimePrepare(t, s)
	if _, err := s.CommitPrepared(ctx, r.OperationID, runtimeEvidence(t, s, r)); err != nil {
		t.Fatal(err)
	}
	q := startgate.Request{Session: rootScanSession, Operation: r.OperationID, Digest: r.Digest, Nonce: scanContractID(860), MessageID: scanContractID(861)}
	gate, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {
		if start != nil {
			start()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, gate, startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce, PermitID: scanContractID(862), ReplyTo: q.MessageID, AuthRev: 1}
}
func runtimeReviewCancelRaw(t *testing.T, session, operation, digest string) []byte {
	t.Helper()
	raw, err := protocol.NewMessage("cancel", session, "", map[string]any{"operation_id": operation, "input_digest": digest})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestRuntimeReviewRejectedCancelHasNoGateSideEffects(t *testing.T) {
	for _, kind := range []string{"old_session", "missing_operation", "missing_claim", "database_unavailable"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, _ := runtimeStore(t)
			r, g, _ := runtimeReviewGate(t, s, nil)
			session, operation := rootScanSession, r.OperationID
			want := ErrFrozen
			switch kind {
			case "old_session":
				session = scanContractID(865)
			case "missing_operation":
				operation = scanContractID(866)
				want = ErrNotFound
			case "missing_claim":
				if _, err := s.db.Exec(`DELETE FROM local_claims WHERE resource_kind='stack'`); err != nil {
					t.Fatal(err)
				}
				want = ErrEvidence
			case "database_unavailable":
				s.db.Close()
				want = ErrStorage
			}
			if _, err := s.CancelExecution(context.Background(), runtimeReviewCancelRaw(t, session, operation, r.Digest)); !errors.Is(err, want) {
				t.Fatalf("cancel returned %v, want %v", err, want)
			}
			if g.State() != startgate.Waiting {
				t.Fatalf("rejected %s cancel changed gate to %s", kind, g.State())
			}
		})
	}
}
func TestRuntimeReviewCancelWriteFailureKeepsStoppedClaimsAndRetries(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	starts := 0
	r, g, permit := runtimeReviewGate(t, s, func() { starts++ })
	if _, err := s.db.Exec(`CREATE TRIGGER runtime_cancel_failure BEFORE INSERT ON execution_events WHEN NEW.kind='result' BEGIN SELECT RAISE(ABORT,'cancel result failed'); END`); err != nil {
		t.Fatal(err)
	}
	raw := runtimeReviewCancelRaw(t, rootScanSession, r.OperationID, r.Digest)
	if _, err := s.CancelExecution(ctx, raw); !errors.Is(err, ErrStorage) {
		t.Fatalf("cancel failure: %v", err)
	}
	state, err := readEventState(ctx, s.db, r.OperationID)
	claims, _ := s.Claims(ctx, r.OperationID)
	if err != nil || state.LastSeq != 0 || len(claims) != 2 || g.State() != startgate.Canceled || starts != 0 {
		t.Fatalf("half canceled/released: %+v claims=%v gate=%v starts=%d err=%v", state, claims, g.State(), starts, err)
	}
	if err = g.Authorize(permit); !errors.Is(err, startgate.ErrCanceled) || starts != 0 {
		t.Fatalf("failed cancel reopened gate: %v starts=%d", err, starts)
	}
	if _, err = s.db.Exec(`DROP TRIGGER runtime_cancel_failure`); err != nil {
		t.Fatal(err)
	}
	decision, err := s.CancelExecution(ctx, raw)
	if err != nil || decision.Disposition != "not_started" || decision.Event == nil || decision.Event.Seq != 1 {
		t.Fatalf("cancel retry: %+v %v", decision, err)
	}
	replay, err := s.CancelExecution(ctx, raw)
	if err != nil || replay.Disposition != "reported" || !bytes.Equal(replay.Event.Body, decision.Event.Body) {
		t.Fatalf("cancel replay: %+v %v", replay, err)
	}
	claims, _ = s.Claims(ctx, r.OperationID)
	if len(claims) != 2 {
		t.Fatalf("cancel retry released claims: %v", claims)
	}
}
func TestRuntimeReviewCancelDoesNotHoldTransactionWhileWaitingForStart(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	entered := make(chan struct{})
	readNow := make(chan struct{})
	readResult := make(chan error, 1)
	var operation string
	finish := make(chan struct{})
	r, g, permit := runtimeReviewGate(t, s, func() {
		close(entered)
		<-readNow
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := s.Get(readCtx, operation)
		readResult <- err
		<-finish
	})
	operation = r.OperationID
	authorizeDone := make(chan error, 1)
	go func() { authorizeDone <- g.Authorize(permit) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("start callback never entered")
	}
	cancelDone := make(chan struct {
		decision CancelDecision
		err      error
	}, 1)
	go func() {
		d, err := s.CancelExecution(ctx, runtimeReviewCancelRaw(t, rootScanSession, r.OperationID, r.Digest))
		cancelDone <- struct {
			decision CancelDecision
			err      error
		}{d, err}
	}()
	select {
	case result := <-cancelDone:
		t.Fatalf("cancel returned before active short callback ended: %+v", result)
	case <-time.After(40 * time.Millisecond):
	}
	close(readNow)
	select {
	case err := <-readResult:
		if err != nil {
			close(finish)
			t.Fatalf("callback SQL blocked by cancel transaction: %v", err)
		}
	case <-time.After(3 * time.Second):
		close(finish)
		t.Fatal("callback read blocked")
	}
	close(finish)
	select {
	case err := <-authorizeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authorize deadlock")
	}
	select {
	case result := <-cancelDone:
		if result.err != nil || result.decision.Disposition != "interrupt_required" {
			t.Fatalf("started cancel: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel deadlock")
	}
}

func TestRuntimeReviewCancelRacesStartWithoutDuplicateExecution(t *testing.T) {
	for i := 0; i < 8; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			s, _, _, _ := runtimeStore(t)
			ctx := context.Background()
			var starts atomic.Int32
			r, g, permit := runtimeReviewGate(t, s, func() { starts.Add(1) })
			raw := runtimeReviewCancelRaw(t, rootScanSession, r.OperationID, r.Digest)
			begin := make(chan struct{})
			authDone := make(chan error, 1)
			cancelDone := make(chan struct {
				decision CancelDecision
				err      error
			}, 1)
			go func() { <-begin; authDone <- g.Authorize(permit) }()
			go func() {
				<-begin
				d, err := s.CancelExecution(ctx, raw)
				cancelDone <- struct {
					decision CancelDecision
					err      error
				}{d, err}
			}()
			close(begin)
			select {
			case <-authDone:
			case <-time.After(3 * time.Second):
				t.Fatal("authorize race deadlock")
			}
			var result struct {
				decision CancelDecision
				err      error
			}
			select {
			case result = <-cancelDone:
			case <-time.After(3 * time.Second):
				t.Fatal("cancel race deadlock")
			}
			if result.err != nil {
				if !errors.Is(result.err, ErrEvidence) {
					t.Fatal(result.err)
				}
				result.decision, result.err = s.CancelExecution(ctx, raw)
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			count := starts.Load()
			if count == 0 {
				if result.decision.Event == nil || result.decision.Event.Seq > 2 {
					t.Fatalf("unstarted race lacks bounded result: %+v", result.decision)
				}
			} else if count == 1 {
				if result.decision.Disposition != "interrupt_required" || result.decision.Event != nil {
					t.Fatalf("started race called not_started: %+v", result.decision)
				}
				if err := s.RecordProcessCreated(ctx, r.OperationID, 95); err != nil {
					t.Fatal(err)
				}
				zero := 0
				fact := ProcessOutcome{BootID: s.BootID(), Outcome: "interrupted", InterruptReason: "canceled", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli())}
				if _, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, ObservationEvidence{}); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Fatalf("execution started %d times", count)
			}
			claims, _ := s.Claims(ctx, r.OperationID)
			if len(claims) != 2 {
				t.Fatalf("cancel race released claims: %v", claims)
			}
		})
	}
}

func TestRuntimeReviewKnownDriftCannotBeHiddenByStatusMetadata(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	ctx := context.Background()
	first, event := runtimeReviewCandidate(t, s, "")
	if _, err := s.Finalize(ctx, releaseRequest(t, first, event.Seq, "succeeded")); err != nil {
		t.Fatal(err)
	}
	_, _ = runtimeReviewCandidate(t, s, scanContractID(870), func(r Record) ObservationEvidence {
		e := runtimeObservation(t, r, "complete")
		e.Conflicts = []ResourceRecord{{Kind: "container", ResourceRef: strings.Repeat("c", 64), ProjectName: r.ProjectName}}
		return e
	})
	binding, err := s.ReadProjectBinding(ctx, first.EndpointID, first.ProjectName)
	if err != nil || binding.Status != "drifted" {
		t.Fatalf("real drift missing: %+v %v", binding, err)
	}
	if _, err = s.db.Exec(`UPDATE project_bindings SET status='active'`); err != nil {
		t.Fatal(err)
	}
	if binding, err = s.ReadProjectBinding(ctx, first.EndpointID, first.ProjectName); !errors.Is(err, ErrEvidence) {
		t.Fatalf("metadata hid authenticated drift: %+v %v", binding, err)
	}
}
