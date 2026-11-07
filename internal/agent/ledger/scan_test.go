package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"docker-manager/internal/protocol"
)

const rootScanSession = "00000000-0000-4000-8000-000000000008"

func rootScanStore(t *testing.T, now func() time.Time) (*Store, string, []byte, JournalKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery.db")
	key := bytes.Repeat([]byte{0x55}, 32)
	jk := JournalKey{ID: "scan-kek", Key: bytes.Repeat([]byte{0x61}, 32)}
	s, err := CreateRecovery(context.Background(), path, testNode, "scan-key", key, jk, RecoveryOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.BindRecoverySessionFixture(context.Background(), rootScanSession); err != nil {
		t.Fatal(err)
	}
	return s, path, key, jk
}

func rootScanRequest(t *testing.T, scope, scan, cursor string, limit int) []byte {
	t.Helper()
	var sid, cur any
	if scan != "" {
		sid = scan
	}
	if cursor != "" {
		cur = cursor
	}
	raw, err := protocol.NewMessage("reconcile", rootScanSession, "", map[string]any{"scope": scope, "scan_id": sid, "cursor": cur, "limit": limit})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type rootScanPage struct {
	ScanID   string            `json:"scan_id"`
	Total    uint64            `json:"total"`
	Records  []json.RawMessage `json:"records"`
	Next     *string           `json:"next_cursor"`
	Complete bool              `json:"complete"`
}

func rootPage(t *testing.T, s *Store, raw []byte) rootScanPage {
	t.Helper()
	out, err := s.Reconcile(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.validator.Validate(out)
	if err != nil || env.Type != "reconcile_reply" {
		t.Fatalf("invalid page %v", err)
	}
	request, _ := s.validator.Validate(raw)
	if env.SessionID == nil || *env.SessionID != rootScanSession || env.ReplyTo == nil || *env.ReplyTo != request.MessageID {
		t.Fatal("page correlation")
	}
	var p rootScanPage
	if err = json.Unmarshal(env.Body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func rootCompleteRequest(t *testing.T, s *Store, id string) []byte {
	t.Helper()
	raw, err := protocol.NewMessage("reconcile_complete", rootScanSession, "", map[string]any{"scan_id": id, "ledger_id": s.Identity().LedgerID})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func rootComplete(t *testing.T, s *Store, id string) {
	t.Helper()
	request := rootCompleteRequest(t, s, id)
	raw, err := s.CompleteReconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "reconcile_completed" {
		t.Fatalf("invalid completed %v", err)
	}
	var body map[string]string
	_ = json.Unmarshal(env.Body, &body)
	if body["scan_id"] != id || body["ledger_id"] != s.Identity().LedgerID {
		t.Fatal(body)
	}
}

func rootEnable(t *testing.T, s *Store) {
	t.Helper()
	page := rootPage(t, s, rootScanRequest(t, "all", "", "", 200))
	rootComplete(t, s, page.ScanID)
}

func TestScanRootEmptyCompletionAndHistoricalReceipt(t *testing.T) {
	s, path, key, jk := rootScanStore(t, nil)
	ctx := context.Background()
	state, err := s.RecoveryScanState(ctx)
	if err != nil || !state.Frozen {
		t.Fatalf("new store not frozen %+v %v", state, err)
	}
	page := rootPage(t, s, rootScanRequest(t, "pending", "", "", 1))
	if page.Total != 0 || len(page.Records) != 0 || !page.Complete || page.Next != nil {
		t.Fatalf("empty page %+v", page)
	}
	rootComplete(t, s, page.ScanID)
	state, err = s.RecoveryScanState(ctx)
	if err != nil || state.Frozen || state.CompletedScanID != page.ScanID {
		t.Fatalf("completion missing %+v %v", state, err)
	}
	newPage := rootPage(t, s, rootScanRequest(t, "all", "", "", 1))
	rootComplete(t, s, page.ScanID)
	state, _ = s.RecoveryScanState(ctx)
	if !state.Frozen || state.ActiveScanID != newPage.ScanID {
		t.Fatal("old receipt ended new scan")
	}
	identity := s.Identity()
	_ = s.Close()
	opened, err := OpenRecovery(ctx, path, identity, key, jk, RecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err = opened.BindRecoverySessionFixture(ctx, rootScanSession); err != nil {
		t.Fatal(err)
	}
	if raw, err := opened.Reconcile(ctx, rootScanRequest(t, "all", newPage.ScanID, "", 1)); !errors.Is(err, ErrScanExpired) || len(raw) != 0 {
		t.Fatalf("reopened active scan %v", err)
	}
	rootComplete(t, opened, page.ScanID)
	state, _ = opened.RecoveryScanState(ctx)
	if !state.Frozen {
		t.Fatal("historical receipt thawed reopened store")
	}
}

func TestScanRootDeadlineAndBudgetFailureStayFrozen(t *testing.T) {
	now := time.Now()
	s, _, _, _ := rootScanStore(t, func() time.Time { return now })
	rootEnable(t, s)
	if _, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t))); err != nil {
		t.Fatal(err)
	}
	page := rootPage(t, s, rootScanRequest(t, "pending", "", "", 1))
	now = now.Add(15*time.Minute - time.Nanosecond)
	rootPage(t, s, rootScanRequest(t, "pending", page.ScanID, "", 1))
	now = now.Add(time.Nanosecond)
	if out, err := s.Reconcile(context.Background(), rootScanRequest(t, "pending", page.ScanID, "", 1)); !errors.Is(err, ErrScanExpired) || len(out) != 0 {
		t.Fatalf("expiry boundary %v", err)
	}
	if s.recovery.snapshot != nil {
		t.Fatal("expired snapshot retained original bodies after access")
	}
	if _, err := s.CompleteReconcile(context.Background(), rootCompleteRequest(t, s, page.ScanID)); !errors.Is(err, ErrScanExpired) {
		t.Fatal(err)
	}
	s.recovery.maxBytes = 1
	if _, err := s.Reconcile(context.Background(), rootScanRequest(t, "all", "", "", 1)); !errors.Is(err, ErrScanBusy) {
		t.Fatalf("budget %v", err)
	}
	state, _ := s.RecoveryScanState(context.Background())
	if !state.Frozen {
		t.Fatal("budget thawed store")
	}
	var claims int
	_ = s.db.QueryRow("SELECT count(*) FROM local_claims").Scan(&claims)
	if claims != 2 {
		t.Fatal("lost claims")
	}
}

func TestScanRootMembersAndOldSnapshotAreAuthenticated(t *testing.T) {
	s, _, _, _ := rootScanStore(t, nil)
	rootEnable(t, s)
	ctx := context.Background()
	r, err := s.RecordReadyFixture(ctx, encodeFixture(t, prepareFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	page := rootPage(t, s, rootScanRequest(t, "all", "", "", 200))
	original := append([]byte(nil), page.Records[0]...)
	event, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1))
	if err != nil {
		t.Fatal(err)
	}
	again := rootPage(t, s, rootScanRequest(t, "all", page.ScanID, "", 200))
	if !bytes.Equal(original, again.Records[0]) {
		t.Fatal("live result changed snapshot")
	}
	if _, err := s.CompleteReconcile(ctx, rootCompleteRequest(t, s, page.ScanID)); !errors.Is(err, ErrScanChanged) {
		t.Fatalf("unacked result accepted %v", err)
	}
	if err = s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, event.Seq, rootScanSession); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE recovery_members SET record_fingerprint=zeroblob(32) WHERE scan_id=?", page.ScanID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteReconcile(ctx, rootCompleteRequest(t, s, page.ScanID)); !errors.Is(err, ErrEvidence) {
		t.Fatalf("corrupt member accepted %v", err)
	}
}

func TestScanRootExhaustedKeyStillReadsAcksAndCompletes(t *testing.T) {
	s, _, _, _ := rootScanStore(t, nil)
	rootEnable(t, s)
	ctx := context.Background()
	r, _, permit := gateReady(t, s)
	if err := s.commitStart(ctx, permit); err != nil {
		t.Fatal(err)
	}
	page := rootPage(t, s, rootScanRequest(t, "all", "", "", 200))
	event, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "normal_exit", 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE event_keys SET uses=? WHERE id=1", maxKeyUses); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadEvent(ctx, r.OperationID, 2)
	if err != nil || !bytes.Equal(got.Body, event.Body) {
		t.Fatalf("exhausted read %v", err)
	}
	again := rootPage(t, s, rootScanRequest(t, "all", page.ScanID, "", 200))
	if !bytes.Equal(again.Records[0], page.Records[0]) {
		t.Fatal("exhausted key changed snapshot")
	}
	if err = s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 2, rootScanSession); err != nil {
		t.Fatal(err)
	}
	rootComplete(t, s, page.ScanID)
	rootComplete(t, s, page.ScanID)
	// A fresh snapshot must also decrypt and enumerate after the quota is
	// exhausted; replaying only the previous in-memory page would not prove it.
	fresh := rootPage(t, s, rootScanRequest(t, "all", "", "", 200))
	if fresh.ScanID == page.ScanID || len(fresh.Records) != 1 || !bytes.Contains(fresh.Records[0], event.Body) {
		t.Fatal("exhausted key could not build a fresh result snapshot")
	}
	rootComplete(t, s, fresh.ScanID)
	var uses int64
	_ = s.db.QueryRow("SELECT uses FROM event_keys WHERE id=1").Scan(&uses)
	if uses != maxKeyUses {
		t.Fatal("scan consumed or refunded KEK budget")
	}
	claims, err := s.Claims(ctx, r.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("scan released claims %v", err)
	}
}

func TestScanRootCloseClearsCachedBodies(t *testing.T) {
	s, _, _, _ := rootScanStore(t, nil)
	rootEnable(t, s)
	if _, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, prepareFixture(t))); err != nil {
		t.Fatal(err)
	}
	rootPage(t, s, rootScanRequest(t, "all", "", "", 200))
	cached := s.recovery.snapshot.records[0]
	if len(cached) == 0 {
		t.Fatal("missing fixture body")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.recovery.snapshot != nil || !bytes.Equal(cached, make([]byte, len(cached))) {
		t.Fatal("closed cache retained plaintext")
	}
}
