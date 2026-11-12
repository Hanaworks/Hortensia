package operations

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func releaseConfirmationRound(t *testing.T) (*Store, Binding, Operation, []byte, []byte) {
	t.Helper()
	ctx := context.Background()
	s, b, op, oldRequest, receipt := releasePending(t)
	if err := s.ReceiveFinalized(ctx, b, receipt); err != nil {
		t.Fatal(err)
	}
	request, err := s.StartRecovery(ctx, b, "all", 10)
	if err != nil {
		t.Fatal(err)
	}
	page := recoveryPageReply(t, s, b, request, 1, []any{rootRecordFor(op)}, nil)
	if _, err = s.ReceiveRecoveryPage(ctx, b, page); err != nil {
		t.Fatal(err)
	}
	state, err := s.ProcessRecovery(ctx, b)
	if err != nil || state.Status != "waiting" {
		t.Fatalf("no current confirmation %+v %v", state, err)
	}
	current, err := s.PrepareFinalize(ctx, b, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return s, b, op, oldRequest, current
}
func TestReleaseManagerCurrentRoundRequiresAuthenticatedConfirmation(t *testing.T) {
	ctx := context.Background()
	s, b, op, _, current := releaseConfirmationRound(t)
	env, _ := s.validator.Validate(current)
	if _, err := s.db.Exec(`UPDATE finalize_requests SET accepted=1 WHERE message_id=?`, env.MessageID); err != nil {
		t.Fatal(err)
	}
	state, err := s.ProcessRecovery(ctx, b)
	if err == nil || state.Status == "ready" {
		t.Fatalf("forged accepted became ready: %+v %v", state, err)
	}
	if !errors.Is(err, ErrEvidence) {
		t.Fatalf("forged accepted not detected as damaged evidence: %v", err)
	}
	if _, err = s.PrepareRecoveryComplete(ctx, b); err == nil {
		t.Fatal("forged accepted passed completion")
	}
	if _, err = s.PrepareFinalize(ctx, b, op.OperationID); !errors.Is(err, ErrEvidence) {
		t.Fatalf("replay accepted corrupted request: %v", err)
	}
}
func TestReleaseManagerConfirmationCannotMoveBetweenRequests(t *testing.T) {
	ctx := context.Background()
	s, b, _, oldRequest, current := releaseConfirmationRound(t)
	oldEnv, _ := s.validator.Validate(oldRequest)
	currentEnv, _ := s.validator.Validate(current)
	var mac []byte
	if err := s.db.QueryRow(`SELECT confirmation_mac FROM finalize_requests WHERE message_id=?`, oldEnv.MessageID).Scan(&mac); err != nil {
		t.Fatal(err)
	}
	if len(mac) != 32 {
		t.Fatalf("missing original confirmation MAC: %d", len(mac))
	}
	if _, err := s.db.Exec(`UPDATE finalize_requests SET accepted=1,confirmation_mac=? WHERE message_id=?`, mac, currentEnv.MessageID); err != nil {
		t.Fatal(err)
	}
	state, err := s.ProcessRecovery(ctx, b)
	if !errors.Is(err, ErrEvidence) || state.Status == "ready" {
		t.Fatalf("copied confirmation became ready: %+v %v", state, err)
	}
}
func TestReleaseManagerReconfirmationWorksWithExhaustedWrappingQuota(t *testing.T) {
	ctx := context.Background()
	s, b, op, oldRequest, current := releaseConfirmationRound(t)
	saved, err := s.ReadFinalized(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	oldEnv, _ := s.validator.Validate(oldRequest)
	currentEnv, _ := s.validator.Validate(current)
	if _, err = s.db.Exec(`UPDATE keys SET uses=1048576`); err != nil {
		t.Fatal(err)
	}
	if err = s.ReceiveFinalized(ctx, b, releaseReceipt(t, s, current)); err != nil {
		t.Fatal(err)
	}
	state, err := s.ProcessRecovery(ctx, b)
	if err != nil || state.Status != "ready" {
		t.Fatalf("confirmation at exhausted quota %+v %v", state, err)
	}
	if mfUses(t, s) != 1048576 || mfCount(t, s, "finalization") != 1 {
		t.Fatal("reconfirmation consumed wrapping use or replaced release")
	}
	var previous, next []byte
	if err = s.db.QueryRow(`SELECT confirmation_mac FROM finalize_requests WHERE message_id=?`, oldEnv.MessageID).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT confirmation_mac FROM finalize_requests WHERE message_id=?`, currentEnv.MessageID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if len(next) != 32 || bytes.Equal(previous, next) {
		t.Fatal("confirmation did not bind distinct request")
	}
	again, err := s.ReadFinalized(ctx, op.OperationID)
	if err != nil || !bytes.Equal(saved, again) {
		t.Fatalf("confirmation replaced original body: %v", err)
	}
	replay, err := s.PrepareFinalize(ctx, b, op.OperationID)
	if err != nil || !bytes.Equal(replay, current) {
		t.Fatalf("confirmed request replay %v", err)
	}
	if err = s.ReceiveFinalized(ctx, b, releaseReceipt(t, s, current)); err != nil {
		t.Fatalf("confirmation replay %v", err)
	}
	if mfUses(t, s) != 1048576 {
		t.Fatal("replay spent wrapping quota")
	}
}
