package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

// Prepare an empty controlled ledger through the same durable recovery path
// used by callers. The fixture supplies the trusted authenticated session.
func recoveryUnfreezeFixture(t *testing.T, s *Store, session string) {
	t.Helper()
	ctx := context.Background()
	if err := s.BindRecoverySessionFixture(ctx, session); err != nil {
		t.Fatal(err)
	}
	request, err := protocol.NewMessage("reconcile", session, "", map[string]any{"scope": "pending", "scan_id": nil, "cursor": nil, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Body struct {
			ScanID   string `json:"scan_id"`
			Total    uint64 `json:"total"`
			Complete bool   `json:"complete"`
		} `json:"body"`
	}
	if err := json.Unmarshal(reply, &page); err != nil || !validID(page.Body.ScanID) || page.Body.Total != 0 || !page.Body.Complete {
		t.Fatalf("empty recovery fixture did not enumerate a complete empty scan: %+v %v", page, err)
	}
	complete, err := protocol.NewMessage("reconcile_complete", session, "", map[string]string{"scan_id": page.Body.ScanID, "ledger_id": s.Identity().LedgerID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteReconcile(ctx, complete); err != nil {
		t.Fatal(err)
	}
	state, err := s.RecoveryScanState(ctx)
	if err != nil || state.Frozen || state.CompletedScanID != page.Body.ScanID {
		t.Fatalf("empty recovery fixture did not persist completion: %+v %v", state, err)
	}
}

func TestRecoveryBarrierPrepareAndGateFreeze(t *testing.T) {
	s, _, _, _ := recoveryStoreFixture(t)
	ctx := context.Background()
	raw := encodeFixture(t, prepareFixture(t))
	if _, err := s.RecordReadyFixture(ctx, raw); !errors.Is(err, ErrFrozen) {
		t.Fatalf("initial prepare: %v", err)
	}
	var count int
	s.db.QueryRow("SELECT count(*) FROM executions").Scan(&count)
	if count != 0 {
		t.Fatal("frozen prepare wrote execution")
	}
	// gateReady uses the standard prepare fixture's session.
	env, err := s.validator.Validate(raw)
	if err != nil {
		t.Fatal(err)
	}
	recoveryUnfreezeFixture(t, s, *env.SessionID)
	r, q, p := gateReady(t, s)
	starts := 0
	g, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) { starts++ })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindRecoverySessionFixture(ctx, gateOtherID); err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(p); !errors.Is(err, startgate.ErrInvalidated) {
		t.Fatalf("old gate: %v", err)
	}
	if starts != 0 {
		t.Fatal("frozen gate started")
	}
	if err := s.commitStart(ctx, p); !errors.Is(err, ErrFrozen) {
		t.Fatalf("direct start: %v", err)
	}
	gateAssertFacts(t, s, r.OperationID, PhaseReady, nil)
	if _, err := s.NewGate(ctx, q, time.Now, func(startgate.Permit) {}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen registration: %v", err)
	}
}

func TestRecoveryBarrierSameSessionDoesNotRefreeze(t *testing.T) {
	s, _, _, _ := recoveryStoreFixture(t)
	recoveryUnfreezeFixture(t, s, testNode)
	if err := s.BindRecoverySessionFixture(context.Background(), testNode); err != nil {
		t.Fatal(err)
	}
	state, err := s.RecoveryScanState(context.Background())
	if err != nil || state.Frozen {
		t.Fatalf("same session refroze: %+v %v", state, err)
	}
}

func TestRecoveryBarrierClosingRejectsWriterChecks(t *testing.T) {
	s, _, _, _ := recoveryStoreFixture(t)
	recoveryUnfreezeFixture(t, s, testNode)
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.closing = false; s.mu.Unlock() }()
	ctx := context.Background()
	if err := s.requireRecoveryWriter(ctx, s.db); !errors.Is(err, ErrStorage) {
		t.Fatalf("closing writer: %v", err)
	}
	if err := s.requireRecoverySession(ctx, s.db, testNode); !errors.Is(err, ErrStorage) {
		t.Fatalf("closing session: %v", err)
	}
	if err := s.requireRecoveryMutation(ctx, s.db, testNode); !errors.Is(err, ErrStorage) {
		t.Fatalf("closing mutation: %v", err)
	}
}

func TestRecoveryBarrierLocalFreezeSurvivesDurableFreezeFailure(t *testing.T) {
	s, _, _, _ := recoveryStoreFixture(t)
	ctx := context.Background()
	env, err := s.validator.Validate(encodeFixture(t, prepareFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	recoveryUnfreezeFixture(t, s, *env.SessionID)
	r, _, p := gateReady(t, s)
	if _, err := s.db.Exec(`CREATE TEMP TRIGGER fail_recovery_freeze BEFORE UPDATE OF current_session ON recovery_control BEGIN SELECT RAISE(ABORT,'freeze failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.BindRecoverySessionFixture(ctx, gateOtherID); !errors.Is(err, ErrStorage) {
		t.Fatalf("freeze fault: %v", err)
	}
	state, err := s.RecoveryScanState(ctx)
	if err != nil || state.Frozen {
		t.Fatalf("failed transaction changed durable freeze: %+v %v", state, err)
	}
	if err := s.commitStart(ctx, p); !errors.Is(err, ErrFrozen) {
		t.Fatalf("local freeze bypass: %v", err)
	}
	if _, err := s.RecordReadyFixture(ctx, encodeFixture(t, prepareFixture(t))); !errors.Is(err, ErrFrozen) {
		t.Fatalf("prepare bypassed local freeze: %v", err)
	}
	gateAssertFacts(t, s, r.OperationID, PhaseReady, nil)
}

func TestRecoveryBarrierResultDeltaAckAndConflict(t *testing.T) {
	s, _, _, _ := recoveryStoreFixture(t)
	ctx := context.Background()
	env, err := s.validator.Validate(encodeFixture(t, prepareFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	recoveryUnfreezeFixture(t, s, *env.SessionID)
	r, _, _ := gateReady(t, s)
	if err := s.BindRecoverySessionFixture(ctx, gateOtherID); err != nil {
		t.Fatal(err)
	}
	raw := resultFixtureFor(t, r, "not_started", 1)
	if _, err := s.RecordResultFixture(ctx, raw); err != nil {
		t.Fatalf("old prepared session result: %v", err)
	}
	var changes int
	if err := s.db.QueryRow("SELECT count(*) FROM recovery_changes").Scan(&changes); err != nil || changes != 1 {
		t.Fatalf("changes=%d %v", changes, err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, r.PreparedSession); !errors.Is(err, ErrFrozen) {
		t.Fatalf("old ACK: %v", err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, gateOtherID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM recovery_changes").Scan(&changes); err != nil || changes != 1 {
		t.Fatal("ack deleted change history")
	}
	// A semantic duplicate with changed authenticated body bytes is a conflict.
	changed := bytes.Replace(raw, []byte(`"phase":`), []byte(`"phase" :`), 1)
	if bytes.Equal(changed, raw) {
		t.Fatal("conflict fixture did not change body")
	}
	if _, err := s.RecordResultFixture(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("same slot conflict: %v", err)
	}
	state, err := s.RecoveryScanState(ctx)
	if err != nil || !state.Dirty {
		t.Fatalf("conflict missing dirty: %+v %v", state, err)
	}
}
