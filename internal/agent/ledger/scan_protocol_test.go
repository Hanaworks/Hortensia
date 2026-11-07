package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

type scanContractPage struct {
	Scope      string            `json:"scope"`
	ScanID     string            `json:"scan_id"`
	LedgerID   string            `json:"ledger_id"`
	Total      uint64            `json:"total"`
	Records    []json.RawMessage `json:"records"`
	NextCursor *string           `json:"next_cursor"`
	Complete   bool              `json:"complete"`
	Error      json.RawMessage   `json:"error"`
}

func scanContractID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
}

func scanContractWire(t *testing.T, kind, session, scan, cursor, scope string, limit int, ledger string) []byte {
	t.Helper()
	body := map[string]any{"scan_id": scan, "ledger_id": ledger}
	if kind == "reconcile" {
		body = map[string]any{"scan_id": nil, "cursor": nil, "scope": scope, "limit": limit}
		if scan != "" {
			body["scan_id"] = scan
		}
		if cursor != "" {
			body["cursor"] = cursor
		}
	}
	raw, err := protocol.NewMessage(kind, session, "", body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func scanContractReply(t *testing.T, s *Store, request, response []byte, kind string) protocol.Envelope {
	t.Helper()
	q, err := s.validator.Validate(request)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.validator.Validate(response)
	if err != nil {
		t.Fatalf("actual response violates wire schema/budget: %v", err)
	}
	if r.Type != kind || r.ReplyTo == nil || *r.ReplyTo != q.MessageID || r.SessionID == nil || q.SessionID == nil || *r.SessionID != *q.SessionID {
		t.Fatal("response lost current request/session correlation")
	}
	return r
}

func scanContractRead(t *testing.T, s *Store, session, scan, cursor, scope string, limit int) (scanContractPage, []byte) {
	t.Helper()
	q := scanContractWire(t, "reconcile", session, scan, cursor, scope, limit, "")
	raw, err := s.Reconcile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	env := scanContractReply(t, s, q, raw, "reconcile_reply")
	var page scanContractPage
	if err := json.Unmarshal(env.Body, &page); err != nil {
		t.Fatal(err)
	}
	if page.ScanID == "" || page.LedgerID != s.Identity().LedgerID || page.Scope != scope || !bytes.Equal(page.Error, []byte("null")) || page.Complete != (page.NextCursor == nil) {
		t.Fatal("success page has invalid scan/ledger/scope/completion metadata")
	}
	return page, env.Body
}

func scanContractComplete(t *testing.T, s *Store, session, scan string) []byte {
	t.Helper()
	q := scanContractWire(t, "reconcile_complete", session, scan, "", "", 0, s.Identity().LedgerID)
	raw, err := s.CompleteReconcile(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return scanContractReply(t, s, q, raw, "reconcile_completed").Body
}

func scanContractCannotComplete(t *testing.T, s *Store, session, scan string, want error) {
	t.Helper()
	q := scanContractWire(t, "reconcile_complete", session, scan, "", "", 0, s.Identity().LedgerID)
	raw, err := s.CompleteReconcile(context.Background(), q)
	if err == nil || raw != nil || (want != nil && !errors.Is(err, want)) {
		t.Fatalf("unexpected completion: bytes=%d err=%v want=%v", len(raw), err, want)
	}
	state, err := s.RecoveryScanState(context.Background())
	if err != nil || !state.Frozen {
		t.Fatalf("failed completion thawed store: %+v %v", state, err)
	}
}

func scanContractStore(t *testing.T, now func() time.Time) (*Store, string) {
	t.Helper()
	s, err := CreateRecovery(context.Background(), filepath.Join(t.TempDir(), "scan.db"), testNode, "scan-contract-ledger", bytes.Repeat([]byte{0x64}, 32), JournalKey{ID: "scan-contract-kek", Key: bytes.Repeat([]byte{0x76}, 32)}, RecoveryOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	session := prepareFixture(t)["session_id"].(string)
	if err := s.BindRecoverySessionFixture(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	// Exercise the same initial freeze/empty scan handshake as a trusted caller.
	// Never modify recovery_control in SQL to manufacture an admitted fixture.
	page, _ := scanContractRead(t, s, session, "", "", "all", 200)
	if page.Total != 0 || len(page.Records) != 0 || !page.Complete {
		t.Fatal("new store is not an empty complete scan")
	}
	scanContractComplete(t, s, session, page.ScanID)
	return s, session
}

func scanContractPrepare(t *testing.T, s *Store, index int) Record {
	t.Helper()
	m := prepareFixture(t)
	body := m["body"].(map[string]any)
	body["operation_id"], body["stack_id"], body["version_id"] = scanContractID(1000+index), scanContractID(2000+index), scanContractID(3000+index)
	changePayload(t, m, func(p map[string]any) {
		p["stack_id"], p["version_id"], p["project_name"] = body["stack_id"], body["version_id"], fmt.Sprintf("scan-contract-%d", index)
	})
	r, err := s.RecordReadyFixture(context.Background(), encodeFixture(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func scanContractStart(t *testing.T, s *Store, r Record) {
	t.Helper()
	nonce, _ := protocol.NewID()
	message, _ := protocol.NewID()
	permit, _ := protocol.NewID()
	q := startgate.Request{Session: r.PreparedSession, Operation: r.OperationID, Digest: r.Digest, Nonce: nonce, MessageID: message}
	count := 0
	g, err := s.NewGate(context.Background(), q, time.Now, func(startgate.Permit) { count++ })
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Authorize(startgate.Permit{Session: q.Session, Operation: q.Operation, Digest: q.Digest, Nonce: q.Nonce, ReplyTo: q.MessageID, PermitID: permit, AuthRev: 1}); err != nil || count != 1 {
		t.Fatalf("controlled start: count=%d error=%v", count, err)
	}
}

func TestScanContractFixedRecordsOrderingAndPaging(t *testing.T) {
	s, session := scanContractStore(t, nil)
	ctx := context.Background()
	records := make(map[int]Record)
	want := make(map[int][]byte)
	for _, i := range []int{3, 1, 2} {
		records[i] = scanContractPrepare(t, s, i)
		if i == 3 {
			if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, records[i], "not_started", 1)); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		want[i], err = s.RecoveryRecord(ctx, records[i].OperationID)
		if err != nil {
			t.Fatal(err)
		}
	}
	first, original := scanContractRead(t, s, session, "", "", "pending", 1)
	if first.Total != 3 || len(first.Records) != 1 || !bytes.Equal(first.Records[0], want[1]) || first.Complete {
		t.Fatal("first page not fixed, sorted, and limited")
	}
	scanContractCannotComplete(t, s, session, first.ScanID, ErrScanIncomplete)
	r := records[2]
	if _, err := s.RecordResultFixture(ctx, resultFixtureFor(t, r, "not_started", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeEvent(ctx, r.OperationID, r.Digest, 1, session); err != nil {
		t.Fatal(err)
	}
	prior := records[3]
	if err := s.AcknowledgeEvent(ctx, prior.OperationID, prior.Digest, 1, session); err != nil {
		t.Fatal(err)
	}
	repeated, repeatedBody := scanContractRead(t, s, session, "", "", "pending", 1)
	if repeated.ScanID != first.ScanID || !bytes.Equal(original, repeatedBody) {
		t.Fatal("first-request retry changed the snapshot or cursor")
	}
	second, _ := scanContractRead(t, s, session, first.ScanID, *first.NextCursor, "pending", 1)
	if second.Total != 3 || len(second.Records) != 1 || !bytes.Equal(second.Records[0], want[2]) || second.Complete {
		t.Fatal("late result/ack changed a previously frozen record")
	}
	third, _ := scanContractRead(t, s, session, first.ScanID, *second.NextCursor, "pending", 1)
	if third.Total != 3 || len(third.Records) != 1 || !bytes.Equal(third.Records[0], want[3]) || !third.Complete {
		t.Fatal("last page omitted/duplicated identities or has incorrect completion")
	}
	scanContractComplete(t, s, session, first.ScanID)
	for _, r := range records {
		claims, err := s.Claims(ctx, r.OperationID)
		if err != nil || len(claims) != 2 {
			t.Fatal("scan completion released operation claims")
		}
	}
}

func TestScanContractRejectsCursorAndRequestBindingErrors(t *testing.T) {
	s, session := scanContractStore(t, nil)
	scanContractPrepare(t, s, 1)
	scanContractPrepare(t, s, 2)
	page, _ := scanContractRead(t, s, session, "", "", "pending", 1)
	for _, tc := range []struct {
		name, session, scan, cursor, scope string
		limit                              int
	}{
		{"fabricated cursor", session, page.ScanID, "unissued-position", "pending", 1},
		{"unknown scan", session, scanContractID(9001), *page.NextCursor, "pending", 1},
		{"scope", session, page.ScanID, *page.NextCursor, "all", 1},
		{"limit", session, page.ScanID, *page.NextCursor, "pending", 2},
		{"session", scanContractID(9002), page.ScanID, *page.NextCursor, "pending", 1},
		{"cursor without scan", session, "", *page.NextCursor, "pending", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := s.Reconcile(context.Background(), scanContractWire(t, "reconcile", tc.session, tc.scan, tc.cursor, tc.scope, tc.limit, ""))
			if err == nil || raw != nil {
				t.Fatalf("invalid association returned success: %v", err)
			}
		})
	}
	// Rejected requests cannot advance the enumeration to the end.
	scanContractCannotComplete(t, s, session, page.ScanID, ErrScanIncomplete)
	last, _ := scanContractRead(t, s, session, page.ScanID, *page.NextCursor, "pending", 1)
	if !last.Complete || len(last.Records) != 1 {
		t.Fatal("valid cursor did not remain usable")
	}
}

func TestScanContractChangesSurviveUpgradeAndReconnect(t *testing.T) {
	s, s1 := scanContractStore(t, nil)
	r := scanContractPrepare(t, s, 1)
	old, _ := scanContractRead(t, s, s1, "", "", "pending", 1)
	if _, err := s.RecordResultFixture(context.Background(), resultFixtureFor(t, r, "not_started", 1)); err != nil {
		t.Fatal(err)
	}
	upgraded, _ := scanContractRead(t, s, s1, "", "", "all", 1)
	if upgraded.ScanID == old.ScanID || !upgraded.Complete {
		t.Fatal("pending-to-all failed to replace the active scan")
	}
	scanContractCannotComplete(t, s, s1, old.ScanID, nil)
	scanContractCannotComplete(t, s, s1, upgraded.ScanID, nil)
	if err := s.AcknowledgeEvent(context.Background(), r.OperationID, r.Digest, 1, s1); err != nil {
		t.Fatal(err)
	}
	s2 := scanContractID(9100)
	if err := s.BindRecoverySessionFixture(context.Background(), s2); err != nil {
		t.Fatal(err)
	}
	continued, _ := scanContractRead(t, s, s2, upgraded.ScanID, "", "all", 1)
	if continued.ScanID != upgraded.ScanID {
		t.Fatal("same-boot reconnection lost the active snapshot")
	}
	scanContractCannotComplete(t, s, s2, upgraded.ScanID, nil)
	if err := s.AcknowledgeEvent(context.Background(), r.OperationID, r.Digest, 1, s1); err == nil {
		t.Fatal("old session accepted an acknowledgement")
	}
	if err := s.AcknowledgeEvent(context.Background(), r.OperationID, r.Digest, 1, s2); err != nil {
		t.Fatal(err)
	}
	scanContractComplete(t, s, s2, upgraded.ScanID)
}

func TestScanContractUpgradeRejectsOldCursorAndDowngrade(t *testing.T) {
	s, session := scanContractStore(t, nil)
	scanContractPrepare(t, s, 1)
	scanContractPrepare(t, s, 2)
	old, _ := scanContractRead(t, s, session, "", "", "pending", 1)
	upgraded, _ := scanContractRead(t, s, session, "", "", "all", 1)
	for _, request := range [][]byte{
		scanContractWire(t, "reconcile", session, old.ScanID, *old.NextCursor, "pending", 1, ""),
		scanContractWire(t, "reconcile", session, upgraded.ScanID, *old.NextCursor, "all", 1, ""),
		scanContractWire(t, "reconcile", session, "", "", "pending", 1, ""),
	} {
		if raw, err := s.Reconcile(context.Background(), request); raw != nil || err == nil {
			t.Fatal("upgrade accepted a stale/cross-scan cursor or downgrade")
		}
	}
	scanContractCannotComplete(t, s, session, old.ScanID, nil)
	scanContractCannotComplete(t, s, session, upgraded.ScanID, ErrScanIncomplete)
	last, _ := scanContractRead(t, s, session, upgraded.ScanID, *upgraded.NextCursor, "all", 1)
	if !last.Complete || len(last.Records) != 1 {
		t.Fatal("rejected old cursor damaged the replacement scan")
	}
	scanContractComplete(t, s, session, upgraded.ScanID)
}

func TestScanContractConflictInvalidatesFrozenView(t *testing.T) {
	s, session := scanContractStore(t, nil)
	r := scanContractPrepare(t, s, 1)
	raw := resultFixtureFor(t, r, "not_started", 1)
	if _, err := s.RecordResultFixture(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	page, _ := scanContractRead(t, s, session, "", "", "pending", 200)
	changed := bytes.Replace(raw, []byte("2026-11-04T04:00:00.000Z"), []byte("2026-11-04T04:00:01.000Z"), 1)
	if _, err := s.RecordResultFixture(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting fixture result: %v", err)
	}
	if err := s.AcknowledgeEvent(context.Background(), r.OperationID, r.Digest, 1, session); err != nil {
		t.Fatal(err)
	}
	scanContractCannotComplete(t, s, session, page.ScanID, ErrScanChanged)
	if wire, err := s.Reconcile(context.Background(), scanContractWire(t, "reconcile", session, page.ScanID, "", "pending", 200, "")); wire != nil || !errors.Is(err, ErrScanChanged) {
		t.Fatalf("dirty scan still served old page: %v", err)
	}
	fresh, _ := scanContractRead(t, s, session, "", "", "pending", 200)
	var view struct {
		Phase  string          `json:"phase"`
		Latest json.RawMessage `json:"latest_result"`
	}
	if err := json.Unmarshal(fresh.Records[0], &view); err != nil || view.Phase != "reconciliation_required" || fresh.ScanID == page.ScanID {
		t.Fatalf("replacement did not expose conflict isolation: %+v %v", view, err)
	}
	event, err := s.ReadEvent(context.Background(), r.OperationID, 1)
	if err != nil || !bytes.Equal(event.Body, view.Latest) {
		t.Fatal("conflict overwrote the original result")
	}
	// Completing this local scan records observation of evidence, not business
	// resolution: blocked evidence and resource claims must remain untouched.
	scanContractComplete(t, s, session, fresh.ScanID)
	claims, err := s.Claims(context.Background(), r.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatal("dirty evidence scan released claims")
	}
}

func TestScanContractCompletedReplayDoesNotThawNewScan(t *testing.T) {
	s, session := scanContractStore(t, nil)
	r := scanContractPrepare(t, s, 1)
	first, _ := scanContractRead(t, s, session, "", "", "pending", 200)
	completed := scanContractComplete(t, s, session, first.ScanID)
	second, _ := scanContractRead(t, s, session, "", "", "all", 200)
	if second.ScanID == first.ScanID {
		t.Fatal("new scan reused a completed scan identity")
	}
	if replay := scanContractComplete(t, s, session, first.ScanID); !bytes.Equal(completed, replay) {
		t.Fatal("completed replay changed its durable body")
	}
	state, err := s.RecoveryScanState(context.Background())
	if err != nil || !state.Frozen || state.ActiveScanID != second.ScanID || state.CompletedScanID != first.ScanID {
		t.Fatalf("old completion altered new scan: %+v %v", state, err)
	}
	claims, err := s.Claims(context.Background(), r.OperationID)
	if err != nil || len(claims) != 2 {
		t.Fatal("completed replay released claims")
	}
	scanContractComplete(t, s, session, second.ScanID)
}

func TestScanContractDeadlineCannotBeRenewed(t *testing.T) {
	now := time.Now()
	s, session := scanContractStore(t, func() time.Time { return now })
	scanContractPrepare(t, s, 1)
	page, _ := scanContractRead(t, s, session, "", "", "pending", 1)
	now = now.Add(14*time.Minute + 59*time.Second)
	retry, _ := scanContractRead(t, s, session, "", "", "pending", 1)
	if retry.ScanID != page.ScanID {
		t.Fatal("retry replaced a still-active scan")
	}
	s2 := scanContractID(9200)
	if err := s.BindRecoverySessionFixture(context.Background(), s2); err != nil {
		t.Fatal(err)
	}
	scanContractRead(t, s, s2, page.ScanID, "", "pending", 1)
	now = now.Add(time.Second)
	if raw, err := s.Reconcile(context.Background(), scanContractWire(t, "reconcile", s2, page.ScanID, "", "pending", 1, "")); raw != nil || !errors.Is(err, ErrScanExpired) {
		t.Fatalf("exact 15-minute boundary remained live: %v", err)
	}
	scanContractCannotComplete(t, s, s2, page.ScanID, ErrScanExpired)
	fresh, _ := scanContractRead(t, s, s2, "", "", "pending", 1)
	if fresh.ScanID == page.ScanID {
		t.Fatal("expired identity revived")
	}
}

func TestScanContractLostMemoryCannotResumeFromPersistentMembers(t *testing.T) {
	s, session := scanContractStore(t, nil)
	scanContractPrepare(t, s, 1)
	page, _ := scanContractRead(t, s, session, "", "", "pending", 1)
	// Deliberately discard only the volatile cache. Durable identities and
	// membership remain, but may never recreate the monotonic lease.
	s.recovery.mu.Lock()
	s.recovery.snapshot.clear()
	s.recovery.snapshot = nil
	s.recovery.mu.Unlock()
	if raw, err := s.Reconcile(context.Background(), scanContractWire(t, "reconcile", session, page.ScanID, "", "pending", 1, "")); raw != nil || !errors.Is(err, ErrScanExpired) {
		t.Fatalf("lost memory resumed from durable members: %v", err)
	}
	scanContractCannotComplete(t, s, session, page.ScanID, ErrScanExpired)
	fresh, _ := scanContractRead(t, s, session, "", "", "pending", 1)
	if fresh.ScanID == page.ScanID || fresh.Total != page.Total {
		t.Fatal("fresh scan revived the old identity or lost records")
	}
}

func TestScanContractDeadlineWhileWaitingForConnection(t *testing.T) {
	var clockMu sync.Mutex
	now := time.Now()
	var armed atomic.Bool
	checked := make(chan struct{}, 1)
	s, session := scanContractStore(t, func() time.Time {
		clockMu.Lock()
		value := now
		clockMu.Unlock()
		if armed.Load() {
			select {
			case checked <- struct{}{}:
			default:
			}
		}
		return value
	})
	scanContractPrepare(t, s, 1)
	scanContractPrepare(t, s, 2)
	first, _ := scanContractRead(t, s, session, "", "", "pending", 1)
	request := scanContractWire(t, "reconcile", session, first.ScanID, *first.NextCursor, "pending", 1, "")
	env, err := s.validator.Validate(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Occupy the actual only connection. The page passes its initial deadline
	// check, then cannot obtain a transaction until this holder is released.
	holder, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	type reply struct {
		raw []byte
		err error
	}
	done := make(chan reply, 1)
	armed.Store(true)
	go func() {
		s.recovery.mu.Lock()
		defer s.recovery.mu.Unlock()
		raw, err := s.scanPage(ctx, env, s.recovery.snapshot, *first.NextCursor)
		done <- reply{raw, err}
	}()
	select {
	case <-checked:
	case <-ctx.Done():
		t.Fatal("page never reached its initial deadline check")
	}
	clockMu.Lock()
	now = now.Add(15 * time.Minute)
	clockMu.Unlock()
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.raw != nil || !errors.Is(result.err, ErrScanExpired) {
			t.Fatalf("page survived expiry while waiting for SQL: bytes=%d error=%v", len(result.raw), result.err)
		}
	case <-ctx.Done():
		t.Fatal("page did not leave the connection wait")
	}
	if snapshot := s.recovery.snapshot; snapshot != nil {
		if snapshot.tail {
			t.Fatal("expired page published last-page completion eligibility")
		}
		if _, issued := snapshot.pages[*first.NextCursor]; issued {
			t.Fatal("expired page was cached as successfully returned")
		}
	}
}

func TestScanContractSnapshotBudgetFailureDoesNotReplaceActive(t *testing.T) {
	for _, budget := range []string{"records", "bytes"} {
		t.Run(budget, func(t *testing.T) {
			s, session := scanContractStore(t, nil)
			scanContractPrepare(t, s, 1)
			scanContractPrepare(t, s, 2)
			page, original := scanContractRead(t, s, session, "", "", "pending", 1)
			if budget == "records" {
				s.recovery.maxRecords = 1
			} else {
				s.recovery.maxBytes = 1
			}
			raw, err := s.Reconcile(context.Background(), scanContractWire(t, "reconcile", session, "", "", "all", 1, ""))
			if err == nil || raw != nil {
				t.Fatal("over-budget replacement published a partial snapshot")
			}
			state, err := s.RecoveryScanState(context.Background())
			if err != nil || !state.Frozen || state.ActiveScanID != page.ScanID || state.Total != page.Total {
				t.Fatalf("failed replacement lost active frozen snapshot: %+v %v", state, err)
			}
			_, replay := scanContractRead(t, s, session, page.ScanID, "", "pending", 1)
			if !bytes.Equal(original, replay) {
				t.Fatal("failed replacement mutated the old snapshot")
			}
		})
	}
}

func TestScanContractBytePaginationPreservesLargeOriginalResults(t *testing.T) {
	s, session := scanContractStore(t, nil)
	ctx := context.Background()
	want := make(map[string][]byte)
	for i := 1; i <= 2; i++ {
		r := scanContractPrepare(t, s, i)
		scanContractStart(t, s, r)
		var message map[string]any
		if err := json.Unmarshal(resultFixtureFor(t, r, "normal_exit", 2), &message); err != nil {
			t.Fatal(err)
		}
		items := make([]any, 4800)
		for j := range items {
			items[j] = map[string]any{"resource_kind": "container", "resource_id": fmt.Sprintf("%064x", j), "name": strings.Repeat("<", 256), "project_name": strings.Repeat("<", 256), "service_name": strings.Repeat("<", 256), "stack_id": nil, "image_id": nil, "state": "running", "health": "none"}
		}
		message["body"].(map[string]any)["observation"].(map[string]any)["items"] = items
		var wire bytes.Buffer
		encoder := json.NewEncoder(&wire)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(message); err != nil {
			t.Fatal(err)
		}
		event, err := s.RecordResultFixture(ctx, wire.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		want[r.OperationID] = event.Body
	}
	totalBytes := 0
	for _, body := range want {
		totalBytes += len(body)
	}
	if totalBytes <= 8<<20 {
		t.Fatal("large-result fixtures do not force byte-based pagination")
	}
	first, _ := scanContractRead(t, s, session, "", "", "all", 200)
	if first.Total != 2 || len(first.Records) != 1 || first.Complete {
		t.Fatal("full-message byte budget did not split large records")
	}
	second, _ := scanContractRead(t, s, session, first.ScanID, *first.NextCursor, "all", 200)
	if second.Total != 2 || len(second.Records) != 1 || !second.Complete {
		t.Fatal("large final page is incomplete or malformed")
	}
	for _, page := range []scanContractPage{first, second} {
		var record struct {
			Operation string          `json:"operation_id"`
			Latest    json.RawMessage `json:"latest_result"`
		}
		if err := json.Unmarshal(page.Records[0], &record); err != nil || !bytes.Equal(record.Latest, want[record.Operation]) {
			t.Fatal("large result body was re-encoded, truncated, or changed")
		}
		delete(want, record.Operation)
	}
	if len(want) != 0 {
		t.Fatal("pagination duplicated a record and omitted another")
	}
}
