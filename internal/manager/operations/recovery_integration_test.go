package operations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

// This local composition deliberately uses real Manager v2 and Agent v3
// stores. Neither barrier is opened by writing frozen/recovery state in SQL.
type recoveryPair struct {
	m                          *Store
	a                          *ledger.Store
	b                          Binding
	path, agentPath, agentBoot string
}

func riID(n int) string  { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }
func riAgentKey() []byte { return bytes.Repeat([]byte{0x83}, 32) }
func riJournalKey() ledger.JournalKey {
	return ledger.JournalKey{ID: "recovery-integration-kek", Key: bytes.Repeat([]byte{0x94}, 32)}
}

func riNewPair(t *testing.T, directory string) *recoveryPair {
	t.Helper()
	return riNewPairClock(t, directory, nil)
}

func riNewPairClock(t *testing.T, directory string, now func() time.Time) *recoveryPair {
	t.Helper()
	ctx := context.Background()
	p := &recoveryPair{path: filepath.Join(directory, "manager.db"), agentPath: filepath.Join(directory, "agent.db")}
	var err error
	p.m, err = CreateRecovery(ctx, p.path, mfManagerID, mfKeys())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.m.Close() })
	prepare := mfExample(t, "prepare")
	node := prepare["body"].(map[string]any)["node_id"].(string)
	p.a, err = ledger.CreateRecovery(ctx, p.agentPath, node, "recovery-integration-ledger", riAgentKey(), riJournalKey(), ledger.RecoveryOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.a.Close() })
	p.b = Binding{NodeID: node, LedgerID: p.a.Identity().LedgerID, SessionID: prepare["session_id"].(string)}
	p.agentBoot = riReadAgentBoot(t, p.agentPath)
	riBind(t, p)
	if err := p.m.CreateUserFixture(ctx, mfUserID, true); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"node.read", "config.read", "stack.deploy"} {
		if err := p.m.SetGrantFixture(ctx, mfUserID, node, capability, true); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// The public Agent fixture has no hello/boot accessor. Read its persisted boot
// as trusted connection metadata, without modifying the Agent database.
func riReadAgentBoot(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("manager-operations-sqlite", databaseURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var boot string
	if err := db.QueryRow("SELECT active_boot FROM recovery_control WHERE id=1").Scan(&boot); err != nil {
		t.Fatal(err)
	}
	return boot
}

func riBind(t *testing.T, p *recoveryPair) {
	t.Helper()
	if err := p.a.BindRecoverySessionFixture(context.Background(), p.b.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := p.m.BindRecoverySessionFixture(context.Background(), p.b, p.agentBoot); err != nil {
		t.Fatal(err)
	}
}

func riFrozen(t *testing.T, s *Store, node string, want bool) {
	t.Helper()
	var frozen bool
	if err := s.db.QueryRow("SELECT frozen FROM nodes WHERE id=?", node).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if frozen != want {
		t.Fatalf("node frozen=%t want=%t", frozen, want)
	}
}

func riExchangePage(t *testing.T, p *recoveryPair, request []byte) (RecoveryState, []byte) {
	t.Helper()
	response, err := p.a.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Agent reconcile: %v", err)
	}
	q, r := mfEnvelope(t, request), mfEnvelope(t, response)
	if r.Type != "reconcile_reply" || r.ReplyTo == nil || *r.ReplyTo != q.MessageID {
		t.Fatal("Agent page lost request correlation")
	}
	state, err := p.m.ReceiveRecoveryPage(context.Background(), p.b, response)
	if err != nil {
		t.Fatalf("Manager receive page: %v", err)
	}
	stored, err := p.m.ReadRecoveryPage(context.Background(), state.RoundID, state.Pages-1)
	if err != nil || !bytes.Equal(stored, r.Body) {
		t.Fatalf("page raw bytes did not round trip: %v", err)
	}
	riFrozen(t, p.m, p.b.NodeID, true)
	return state, response
}

func riScan(t *testing.T, p *recoveryPair, scope string, limit int) RecoveryState {
	t.Helper()
	request, err := p.m.StartRecovery(context.Background(), p.b, scope, limit)
	if err != nil {
		t.Fatal(err)
	}
	for {
		state, _ := riExchangePage(t, p, request)
		if state.NextCursor == "" {
			return state
		}
		request, err = p.m.NextRecoveryRequest(context.Background(), p.b)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func riPrepareComplete(t *testing.T, p *recoveryPair) ([]byte, []byte) {
	t.Helper()
	state, err := p.m.ProcessRecovery(context.Background(), p.b)
	if err != nil || state.Status != "ready" {
		t.Fatalf("business not ready: %+v %v", state, err)
	}
	request, err := p.m.PrepareRecoveryComplete(context.Background(), p.b)
	if err != nil {
		t.Fatal(err)
	}
	riFrozen(t, p.m, p.b.NodeID, true)
	response, err := p.a.CompleteReconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Agent complete: %v", err)
	}
	return request, response
}

func riComplete(t *testing.T, p *recoveryPair) []byte {
	t.Helper()
	_, response := riPrepareComplete(t, p)
	state, err := p.m.ReceiveRecoveryCompleted(context.Background(), p.b, response)
	if err != nil || state.Status != "completed" {
		t.Fatalf("Manager completed: %+v %v", state, err)
	}
	riFrozen(t, p.m, p.b.NodeID, false)
	agentState, err := p.a.RecoveryScanState(context.Background())
	if err != nil || agentState.Frozen {
		t.Fatalf("Agent remains frozen: %+v %v", agentState, err)
	}
	return response
}

func riOpenPair(t *testing.T) *recoveryPair {
	t.Helper()
	p := riNewPair(t, t.TempDir())
	riScan(t, p, "all", 200)
	riComplete(t, p)
	return p
}

func riPrepare(t *testing.T, p *recoveryPair, index int, agent bool) (Operation, []byte) {
	t.Helper()
	message := mfExample(t, "prepare")
	message["session_id"] = p.b.SessionID
	body := message["body"].(map[string]any)
	body["operation_id"], body["stack_id"], body["version_id"] = riID(1000+index), riID(2000+index), riID(3000+index)
	payload, err := base64.StdEncoding.DecodeString(body["payload_b64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		t.Fatal(err)
	}
	data["stack_id"], data["version_id"], data["project_name"] = body["stack_id"], body["version_id"], fmt.Sprintf("recovery-pair-%d", index)
	payload = mfEncode(t, data)
	body["payload_b64"] = base64.StdEncoding.EncodeToString(payload)
	body["input_digest"], err = p.m.InputDigest(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw := mfEncode(t, message)
	op, err := p.m.AcceptFixture(context.Background(), p.b, mfUserID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if agent {
		if _, err := p.a.RecordReadyFixture(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
		if err := p.m.RecordReadyFixture(context.Background(), p.b, op.OperationID, op.Digest); err != nil {
			t.Fatal(err)
		}
		op, err = p.m.Get(context.Background(), op.OperationID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return op, raw
}

func riAuthorize(t *testing.T, p *recoveryPair, op Operation, starts *int) Operation {
	t.Helper()
	request := mfRequest(t, p.b, op)
	q := mfEnvelope(t, request)
	var req struct {
		Nonce string `json:"start_nonce"`
	}
	if err := json.Unmarshal(q.Body, &req); err != nil {
		t.Fatal(err)
	}
	gate, err := p.a.NewGate(context.Background(), startgate.Request{Session: p.b.SessionID, Operation: op.OperationID, Digest: op.Digest, Nonce: req.Nonce, MessageID: q.MessageID}, time.Now, func(startgate.Permit) { *starts++ })
	if err != nil {
		t.Fatal(err)
	}
	wire, err := p.m.AuthorizeStart(context.Background(), p.b, request)
	if err != nil {
		t.Fatal(err)
	}
	e := mfEnvelope(t, wire)
	var permit struct {
		ID       string `json:"permit_id"`
		Revision uint64 `json:"auth_rev"`
	}
	if err := json.Unmarshal(e.Body, &permit); err != nil {
		t.Fatal(err)
	}
	if err := gate.Authorize(startgate.Permit{Session: p.b.SessionID, Operation: op.OperationID, Digest: op.Digest, Nonce: req.Nonce, ReplyTo: *e.ReplyTo, PermitID: permit.ID, AuthRev: permit.Revision}); err != nil {
		t.Fatal(err)
	}
	op, err = p.m.Get(context.Background(), op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func riDeliverEvent(t *testing.T, p *recoveryPair, event ledger.Event) {
	t.Helper()
	wire, err := protocol.NewMessage(event.Kind, p.b.SessionID, "", event.Body)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := p.m.ReceiveRecoveryEvent(context.Background(), p.b, wire)
	if err != nil {
		t.Fatal(err)
	}
	in, out := mfEnvelope(t, wire), mfEnvelope(t, ack)
	var acknowledged struct {
		Operation string `json:"operation_id"`
		Seq       uint64 `json:"event_seq"`
	}
	if err := json.Unmarshal(out.Body, &acknowledged); err != nil {
		t.Fatal(err)
	}
	if out.Type != "ack" || out.ReplyTo == nil || *out.ReplyTo != in.MessageID || acknowledged.Operation != event.OperationID || acknowledged.Seq != event.Seq {
		t.Fatal("ack lost event identity/correlation")
	}
	stored, err := p.m.ReadEvent(context.Background(), event.OperationID, event.Seq)
	if err != nil || !bytes.Equal(stored.Body, event.Body) {
		t.Fatalf("ack before exact event evidence: %v", err)
	}
	if err := p.a.AcknowledgeEvent(context.Background(), event.OperationID, event.Digest, event.Seq, p.b.SessionID); err != nil {
		t.Fatal(err)
	}
}

func riAssertWaiting(t *testing.T, p *recoveryPair, operation string) {
	t.Helper()
	state, err := p.m.ProcessRecovery(context.Background(), p.b)
	if err != nil || state.Status != "waiting" {
		t.Fatalf("expected business wait: %+v %v", state, err)
	}
	if wire, err := p.m.PrepareRecoveryComplete(context.Background(), p.b); err == nil || len(wire) != 0 {
		t.Fatal("unfinished operation prepared completion")
	}
	for _, claims := range []func(context.Context, string) (int, error){
		func(ctx context.Context, id string) (int, error) { c, e := p.m.Claims(ctx, id); return len(c), e },
		func(ctx context.Context, id string) (int, error) { c, e := p.a.Claims(ctx, id); return len(c), e },
	} {
		if n, err := claims(context.Background(), operation); err != nil || n != 2 {
			t.Fatalf("waiting lost claims: %d %v", n, err)
		}
	}
	riFrozen(t, p.m, p.b.NodeID, true)
}

func TestRecoveryIntegrationEmptyHandshakeAndStaleCompleted(t *testing.T) {
	for _, change := range []string{"session", "agent_boot", "manager_boot"} {
		t.Run(change, func(t *testing.T) {
			p := riNewPair(t, t.TempDir())
			other := Binding{NodeID: riID(7001), LedgerID: riID(7002), SessionID: riID(7003)}
			if err := p.m.BindRecoverySessionFixture(context.Background(), other, riID(7004)); err != nil {
				t.Fatal(err)
			}
			state := riScan(t, p, "all", 1)
			if state.Total != 0 || state.Seen != 0 || state.Pages != 1 {
				t.Fatalf("empty page: %+v", state)
			}
			completed := riComplete(t, p)
			riFrozen(t, p.m, other.NodeID, true)
			if _, err := p.m.ReceiveRecoveryCompleted(context.Background(), p.b, completed); err != nil {
				t.Fatalf("same-round replay: %v", err)
			}
			switch change {
			case "session":
				p.b.SessionID = riID(7010)
			case "agent_boot":
				identity := p.a.Identity()
				if err := p.a.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				p.a, err = ledger.OpenRecovery(context.Background(), p.agentPath, identity, riAgentKey(), riJournalKey(), ledger.RecoveryOptions{})
				if err != nil {
					t.Fatal(err)
				}
				p.agentBoot = riReadAgentBoot(t, p.agentPath)
			case "manager_boot":
				identity := p.m.Identity()
				if err := p.m.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				p.m, err = OpenRecovery(context.Background(), p.path, identity, mfKeys())
				if err != nil {
					t.Fatal(err)
				}
			}
			riBind(t, p)
			riFrozen(t, p.m, p.b.NodeID, true)
			if _, err := p.m.ReceiveRecoveryCompleted(context.Background(), p.b, completed); err == nil {
				t.Fatal("old completed accepted across binding/boot change")
			}
			riFrozen(t, p.m, p.b.NodeID, true)
			riScan(t, p, "all", 1)
			riComplete(t, p)
			riFrozen(t, p.m, other.NodeID, true)
		})
	}
}

func TestRecoveryIntegrationPendingToAllAbsenceRelease(t *testing.T) {
	p := riOpenPair(t)
	op, _ := riPrepare(t, p, 1, false)
	pending := riScan(t, p, "pending", 1)
	state, err := p.m.ProcessRecovery(context.Background(), p.b)
	if err != nil || state.Status != "need_all" {
		t.Fatalf("missing pending evidence: %+v %v", state, err)
	}
	if c, err := p.m.Claims(context.Background(), op.OperationID); err != nil || len(c) != 2 {
		t.Fatal("pending omission released claims")
	}
	all := riScan(t, p, "all", 1)
	if all.RoundID == pending.RoundID || all.ScanID == pending.ScanID {
		t.Fatal("all upgrade reused pending evidence")
	}
	riComplete(t, p)
	riAssertAbsenceReleased(t, p.m, op.OperationID, all.RoundID)
	if _, err := p.a.Get(context.Background(), op.OperationID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("absence manufactured Agent record: %v", err)
	}
}

func riAssertAbsenceReleased(t *testing.T, s *Store, id, round string) {
	t.Helper()
	op, err := s.Get(context.Background(), id)
	if err != nil || op.State != "canceled" || !op.CancelRequested || op.ReleaseStatus != "released" || op.PermitID != "" || op.MaxEventSeq != 0 {
		t.Fatalf("invalid absence projection: %+v %v", op, err)
	}
	claims, err := s.Claims(context.Background(), id)
	if err != nil || len(claims) != 0 {
		t.Fatalf("absence retained claims: %+v %v", claims, err)
	}
	var proof string
	if err := s.db.QueryRow("SELECT round_id FROM absence_releases WHERE operation_id=?", id).Scan(&proof); err != nil || proof != round {
		t.Fatalf("missing durable all-scan proof: %q %v", proof, err)
	}
	intents, err := s.Intents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 2 {
		t.Fatalf("absence expected prepare/cancel intents: %+v", intents)
	}
	for _, intent := range intents {
		if (intent.Kind != "prepare" && intent.Kind != "cancel") || intent.State != "suppressed" {
			t.Fatalf("absence left delivery or fabricated finalize: %+v", intent)
		}
	}
	if _, err := s.ReadEvent(context.Background(), id, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absence manufactured event: %v", err)
	}
}

func TestRecoveryIntegrationReadyCancelResultStillWaitsForFinalize(t *testing.T) {
	p := riOpenPair(t)
	op, _ := riPrepare(t, p, 2, true)
	riScan(t, p, "all", 1)
	riAssertWaiting(t, p, op.OperationID)
	op, err := p.m.Get(context.Background(), op.OperationID)
	if err != nil || !op.CancelRequested || op.PermitID != "" {
		t.Fatalf("ready was not canceled: %+v %v", op, err)
	}
	intents, err := p.m.Intents(context.Background(), op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 2 {
		t.Fatalf("cancellation missing prepare/cancel intents: %+v", intents)
	}
	cancels := 0
	for _, intent := range intents {
		if intent.Kind == "prepare" && intent.State != "suppressed" && intent.State != "confirmed" {
			t.Fatal("cancellation retained prepare delivery")
		}
		if intent.Kind == "cancel" {
			cancels++
			if intent.State != "pending" {
				t.Fatal("cancellation missing pending delivery")
			}
		}
	}
	if cancels != 1 {
		t.Fatalf("cancellation has %d cancel intents", cancels)
	}
	// The controlled Agent fixture supplies a not_started fact; it does not
	// pretend that an unimplemented network cancel handler ran.
	event, err := p.a.RecordResultFixture(context.Background(), eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "canceled_before_start"))
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, event)
	riAssertWaiting(t, p, op.OperationID)
	op, err = p.m.Get(context.Background(), op.OperationID)
	if err != nil || op.State != "canceled" || op.ReleaseStatus != "pending" || op.LatestResultSeq != 1 {
		t.Fatalf("cancel result projection: %+v %v", op, err)
	}
}

func TestRecoveryIntegrationLateEventsBeatFixedPageWithoutFabricatingStarted(t *testing.T) {
	p := riOpenPair(t)
	op, _ := riPrepare(t, p, 3, true)
	starts := 0
	op = riAuthorize(t, p, op, &starts)
	// Capture START_COMMITTED, but do not deliver the real started event yet.
	riScan(t, p, "all", 1)
	result, err := p.a.RecordResultFixture(context.Background(), eventResult(t, p.m, p.b, op, "normal_exit", 0, "complete", "consistent", ""))
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, result)
	if _, err := p.m.ReadEvent(context.Background(), op.OperationID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("result fabricated started evidence: %v", err)
	}
	riAssertWaiting(t, p, op.OperationID)
	stored, err := p.m.Get(context.Background(), op.OperationID)
	if err != nil || stored.State != "succeeded" || stored.LatestResultSeq != 2 || stored.ReleaseStatus != "pending" {
		t.Fatalf("old page overwrote live result: %+v %v", stored, err)
	}
	started, err := p.a.ReadEvent(context.Background(), op.OperationID, 1)
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, started)
	riDeliverEvent(t, p, result)
	riAssertWaiting(t, p, op.OperationID)
	mfAssertFinalize(t, p.m, op.OperationID, true)
	if starts != 1 || mfCount(t, p.m, "operation_events") != 2 {
		t.Fatalf("duplicate execution/events: starts=%d events=%d", starts, mfCount(t, p.m, "operation_events"))
	}
}

func TestRecoveryIntegrationLateUnknownEventBlocksPreparedComplete(t *testing.T) {
	p := riNewPair(t, t.TempDir())
	riScan(t, p, "all", 1)
	_, response := riPrepareComplete(t, p)
	unknown := Operation{OperationID: riID(9001), Digest: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x55}, 32))}
	wire := eventResult(t, p.m, p.b, unknown, "not_started", 0, "", "not_checked", "canceled_before_start")
	ack, err := p.m.ReceiveRecoveryEvent(context.Background(), p.b, wire)
	if err != nil || len(ack) == 0 {
		t.Fatalf("unknown event not durably acknowledged: %v", err)
	}
	if mfCount(t, p.m, "recovery_cases") != 1 || mfCount(t, p.m, "operations") != 0 {
		t.Fatal("unknown event discarded or fabricated operation")
	}
	if _, err := p.m.ReceiveRecoveryCompleted(context.Background(), p.b, response); err == nil {
		t.Fatal("late case bypassed completion recheck")
	}
	riFrozen(t, p.m, p.b.NodeID, true)
}

func TestRecoveryIntegrationMultiplePagesKeepSnapshotAndMergeLiveResult(t *testing.T) {
	p := riOpenPair(t)
	for _, index := range []int{32, 31, 33} {
		riPrepare(t, p, index, true)
	}
	request, err := p.m.StartRecovery(context.Background(), p.b, "all", 1)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := riExchangePage(t, p, request)
	if first.Total != 3 || first.Seen != 1 || first.Pages != 1 || first.NextCursor == "" {
		t.Fatalf("invalid first page: %+v", first)
	}
	op, err := p.m.Get(context.Background(), riID(1032))
	if err != nil {
		t.Fatal(err)
	}
	event, err := p.a.RecordResultFixture(context.Background(), eventResult(t, p.m, p.b, op, "not_started", 0, "", "not_checked", "preflight_failed"))
	if err != nil {
		t.Fatal(err)
	}
	riDeliverEvent(t, p, event)
	for page := 2; page <= 3; page++ {
		request, err = p.m.NextRecoveryRequest(context.Background(), p.b)
		if err != nil {
			t.Fatal(err)
		}
		retry, err := p.m.NextRecoveryRequest(context.Background(), p.b)
		if err != nil || !bytes.Equal(request, retry) {
			t.Fatalf("pending page request was not replayed: %v", err)
		}
		state, _ := riExchangePage(t, p, request)
		if state.RoundID != first.RoundID || state.ScanID != first.ScanID || state.Total != 3 || state.Seen != page || state.Pages != page || (state.NextCursor == "") != (page == 3) {
			t.Fatalf("page %d lost snapshot progress: %+v", page, state)
		}
	}
	state, err := p.m.ProcessRecovery(context.Background(), p.b)
	if err != nil || state.Status != "waiting" {
		t.Fatalf("nonempty business scan completed: %+v %v", state, err)
	}
	stored, err := p.m.Get(context.Background(), op.OperationID)
	if err != nil || stored.State != "failed" || stored.ReleaseStatus != "pending" || stored.LatestResultSeq != 1 {
		t.Fatalf("old ready page overwrote live result: %+v %v", stored, err)
	}
	var members int
	if err := p.m.db.QueryRow("SELECT count(*) FROM recovery_members WHERE round_id=?", first.RoundID).Scan(&members); err != nil || members != 3 {
		t.Fatalf("pagination lost/duplicated membership: %d %v", members, err)
	}
	for _, index := range []int{31, 32, 33} {
		claims, err := p.m.Claims(context.Background(), riID(1000+index))
		if err != nil || len(claims) != 2 {
			t.Fatalf("page processing released claims for %d", index)
		}
	}
	riFrozen(t, p.m, p.b.NodeID, true)
}

func TestRecoveryIntegrationExpiredCompletionCanRestartFromDurableFailure(t *testing.T) {
	now := time.Now()
	p := riNewPairClock(t, t.TempDir(), func() time.Time { return now })
	old := riScan(t, p, "all", 1)
	if state, err := p.m.ProcessRecovery(context.Background(), p.b); err != nil || state.Status != "ready" {
		t.Fatalf("empty scan not ready: %+v %v", state, err)
	}
	request, err := p.m.PrepareRecoveryComplete(context.Background(), p.b)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Minute)
	if response, err := p.a.CompleteReconcile(context.Background(), request); !errors.Is(err, ledger.ErrScanExpired) || len(response) != 0 {
		t.Fatalf("Agent deadline not enforced: %v", err)
	}
	state, err := p.m.RecordRecoveryFailureFixture(context.Background(), p.b, mfEnvelope(t, request).MessageID, "scan_expired")
	if err != nil || state.Status != "expired" {
		t.Fatalf("completion failure not retained: %+v %v", state, err)
	}
	riFrozen(t, p.m, p.b.NodeID, true)
	fresh := riScan(t, p, "all", 1)
	if fresh.RoundID == old.RoundID || fresh.ScanID == old.ScanID {
		t.Fatal("expired completion identity revived")
	}
	riComplete(t, p)
}
