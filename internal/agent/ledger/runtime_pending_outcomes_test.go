package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRuntimePendingProcessOutcomesSurviveReopenUntilResult(t *testing.T) {
	ctx := context.Background()
	s, path, key, jk := runtimeStore(t)
	r := runtimePrepare(t, s)
	runtimeStart(t, s, r, nil)
	if err := s.RecordProcessCreated(ctx, r.OperationID, 123); err != nil {
		t.Fatal(err)
	}
	refs, err := s.PendingProcessOutcomeRefs(ctx, 1)
	if err != nil || len(refs) != 0 {
		t.Fatalf("invented exit: %v %v", refs, err)
	}
	exit := 7
	f := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &exit, FinishedAt: timestamp(time.Now().UnixMilli()), Output: []byte("original exit output\n"), OutputTruncated: true}
	if err = s.RecordProcessExit(ctx, r.OperationID, f); err != nil {
		t.Fatal(err)
	}
	refs, err = s.PendingProcessOutcomeRefs(ctx, 1)
	if err != nil || len(refs) != 1 || refs[0] != r.OperationID {
		t.Fatalf("missing durable exit: %v %v", refs, err)
	}
	identity := s.Identity()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenRuntime(ctx, path, identity, key, jk, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	refs, err = s.PendingProcessOutcomeRefs(ctx, 32)
	if err != nil || len(refs) != 1 || refs[0] != r.OperationID {
		t.Fatalf("reopen lost durable exit: %v %v", refs, err)
	}
	got, err := s.ReadProcessOutcome(ctx, refs[0])
	if err != nil || !sameOutcome(got, f) {
		t.Fatal("original process fact changed", err)
	}
	if _, err = s.RecordProcessOutcome(ctx, r.OperationID, got, runtimeObservation(t, r, "error")); err != nil {
		t.Fatal(err)
	}
	refs, err = s.PendingProcessOutcomeRefs(ctx, 32)
	if err != nil || len(refs) != 0 {
		t.Fatalf("existing result still pending: %v %v", refs, err)
	}
}

func TestRuntimePendingProcessOutcomesRejectInvalidLimit(t *testing.T) {
	s, _, _, _ := runtimeStore(t)
	for _, limit := range []int{-1, 0, 33} {
		if _, err := s.PendingProcessOutcomeRefs(context.Background(), limit); !errors.Is(err, ErrInput) {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
}

func TestRuntimePendingProcessOutcomesAuthenticateEvidence(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE runtime_documents SET body_cipher=zeroblob(length(body_cipher)) WHERE kind='process'`,
		`UPDATE process_facts SET owner_boot='different-owner'`,
		`UPDATE process_facts SET spawn_state='created'`,
		`DELETE FROM local_claims`,
		`DELETE FROM execution_events WHERE kind='started'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			s, _, _, _ := runtimeStore(t)
			r := runtimePrepare(t, s)
			runtimeStart(t, s, r, nil)
			if err := s.RecordProcessCreated(ctx, r.OperationID, 123); err != nil {
				t.Fatal(err)
			}
			code := 0
			if err := s.RecordProcessExit(ctx, r.OperationID, ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &code, FinishedAt: timestamp(time.Now().UnixMilli())}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PendingProcessOutcomeRefs(ctx, 32); err == nil {
				t.Fatal("damaged exit accepted or silently omitted")
			}
		})
	}
}

func TestRuntimeEveryOutcomePathAuthenticatesExitMetadata(t *testing.T) {
	for _, mutation := range []string{`UPDATE process_facts SET owner_boot='different-owner'`, `UPDATE process_facts SET spawn_state='created'`} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			s, _, _, _ := runtimeStore(t)
			r := runtimePrepare(t, s)
			runtimeStart(t, s, r, nil)
			if err := s.RecordProcessCreated(ctx, r.OperationID, 123); err != nil {
				t.Fatal(err)
			}
			code := 0
			fact := ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &code, FinishedAt: timestamp(time.Now().UnixMilli())}
			if err := s.RecordProcessExit(ctx, r.OperationID, fact); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReadProcessOutcome(ctx, r.OperationID); err == nil {
				t.Error("direct read bypassed exit metadata authentication")
			}
			if _, err := s.RecordProcessOutcome(ctx, r.OperationID, fact, runtimeObservation(t, r, "error")); err == nil {
				t.Error("final result transaction bypassed exit metadata authentication")
			}
			state, err := readEventState(ctx, s.db, r.OperationID)
			if err != nil || state.LastSeq != 1 || state.LatestResultSeq.Valid {
				t.Errorf("damaged exit generated result: %+v %v", state, err)
			}
			claims, err := s.Claims(ctx, r.OperationID)
			if err != nil || len(claims) != 2 {
				t.Errorf("claims changed: %v %v", claims, err)
			}
		})
	}
}
