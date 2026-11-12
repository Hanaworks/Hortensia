package agenttransport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agentwire"
	"docker-manager/internal/manager/operations"
	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

// The identity, Apply, recovery, gate and event stores are production APIs.
// Preparation and the deliberately failed spawn are controlled test inputs;
// this test does not claim a Docker process or a successful deployment.
type transportPair struct {
	ctx     context.Context
	m       *operations.Store
	a       *ledger.Store
	h       *Handler
	server  *httptest.Server
	login   operations.BrowserLogin
	node    string
	user    string
	secret  string
	profile ledger.RuntimeProfile
}

func newTransportPair(t *testing.T) *transportPair {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	m, err := operations.CreateRuntimeService(ctx, filepath.Join(dir, "manager.db"), id(t), operations.Keys{DigestID: "digest", EncryptionID: "encryption", DigestKey: bytes.Repeat([]byte{11}, 32), EncryptionKey: bytes.Repeat([]byte{12}, 32)}, operations.Bootstrap{Username: "admin", Password: "transport test password"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	login, err := m.Login(ctx, "admin", "transport test password")
	if err != nil {
		t.Fatal(err)
	}
	f := &transportPair{ctx: ctx, m: m, login: login, user: reply(t, login.Reply)["user_id"].(string)}
	f.node = f.service(t, "POST", "/api/v1/nodes", map[string]any{"display_name": "transport node"}, "")["node_id"].(string)
	f.grant(t, []string{"node.read", "config.read", "config.write", "stack.deploy", "operation.read", "operation.output.read", "operation.cancel"}, 0)
	f.profile = ledger.RuntimeProfile{EndpointID: id(t), PolicyRevision: 1, Fingerprint: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{15}, 32)), EngineID: "controlled-preparation-only", DockerVersion: "controlled", ComposeVersion: "controlled"}
	f.a, err = ledger.CreateRuntime(ctx, filepath.Join(dir, "agent.db"), f.node, "ledger", bytes.Repeat([]byte{13}, 32), ledger.JournalKey{ID: "journal", Key: bytes.Repeat([]byte{14}, 32)}, ledger.RuntimeOptions{Profile: f.profile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.a.Close() })
	token := f.service(t, "POST", "/api/v1/nodes/"+f.node+"/enrollment-tokens", map[string]any{}, "")["token"].(string)
	f.secret = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{16}, 32))
	if _, err := m.EnrollNode(ctx, token, operations.EnrollmentRequest{NodeID: f.node, LedgerID: f.a.Identity().LedgerID, CredentialSecret: f.secret}); err != nil {
		t.Fatal(err)
	}
	f.h, err = New(m)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewTLSServer(f.h)
	t.Cleanup(f.server.Close)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		if err := f.h.Close(closeCtx); err != nil {
			t.Error("transport cleanup", err)
		}
	})
	return f
}

func (f *transportPair) service(t *testing.T, method, path string, body any, match string) map[string]any {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	key := ""
	if method != "GET" {
		key = id(t)
	}
	r, err := f.m.DoService(f.ctx, f.login.Session, operations.ServiceRequest{Method: method, Path: path, Body: raw, IdempotencyKey: key, IfMatch: match})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return reply(t, r)
}

func (f *transportPair) grant(t *testing.T, caps []string, revision uint64) {
	t.Helper()
	f.service(t, "PUT", "/api/v1/nodes/"+f.node+"/grants/"+f.user, map[string]any{"capabilities": caps}, fmt.Sprintf(`"%d"`, revision))
}

func (f *transportPair) hello(t *testing.T, node string) []byte {
	t.Helper()
	return transportMessage(t, "hello", "", "", map[string]any{"node_id": node, "ledger_id": f.a.Identity().LedgerID, "boot_id": f.a.BootID(), "agent_version": "controlled-transport-test", "compose_version": "controlled", "capabilities": []string{"compose.apply.v1", "config.bundle.v1"}, "execution_profile": map[string]any{"docker_endpoint_id": f.profile.EndpointID, "policy_revision": f.profile.PolicyRevision}})
}

func transportMessage(t *testing.T, kind, session, replyTo string, body any) []byte {
	t.Helper()
	raw, err := protocol.NewMessage(kind, session, replyTo, body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func transportEnvelope(t *testing.T, raw []byte) protocol.Envelope {
	t.Helper()
	var e protocol.Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func (f *transportPair) dial(t *testing.T) (*agentwire.Peer, operations.Binding) {
	t.Helper()
	hello := f.hello(t, f.node)
	p, welcome, err := agentwire.Dial(f.ctx, agentwire.ClientConfig{URL: "wss" + strings.TrimPrefix(f.server.URL, "https") + "/agent/v1/connect", Token: f.secret, Hello: hello, RootCAs: f.server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs})
	if err != nil {
		t.Fatal("TLS dial", err)
	}
	t.Cleanup(p.Close)
	if err := f.a.BindManagerSession(f.ctx, welcome, transportEnvelope(t, hello).MessageID); err != nil {
		t.Fatal(err)
	}
	return p, operations.Binding{NodeID: f.node, LedgerID: f.a.Identity().LedgerID, SessionID: p.SessionID()}
}

func transportWrite(t *testing.T, ctx context.Context, p *agentwire.Peer, raw []byte) {
	t.Helper()
	if err := p.Write(ctx, raw); err != nil {
		t.Fatal("TLS write", err)
	}
}

func transportRead(t *testing.T, ctx context.Context, p *agentwire.Peer, kind string) ([]byte, protocol.Envelope) {
	t.Helper()
	raw, e, err := p.Read(ctx)
	if err != nil {
		t.Fatalf("read %s: %v", kind, err)
	}
	if e.Type != kind {
		t.Fatalf("wanted %s, received %s", kind, e.Type)
	}
	return raw, e
}

func transportEventually(t *testing.T, ctx context.Context, description string, check func() bool) {
	t.Helper()
	limit := time.NewTimer(3 * time.Second)
	defer limit.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(description, ctx.Err())
		case <-limit.C:
			t.Fatal(description)
		case <-tick.C:
		}
	}
}

func (f *transportPair) recover(t *testing.T, p *agentwire.Peer, b operations.Binding) string {
	t.Helper()
	for count := 0; count < 20; count++ {
		raw, e, err := p.Read(f.ctx)
		if err != nil {
			t.Fatal("recovery read", err)
		}
		var answer []byte
		switch e.Type {
		case "reconcile":
			answer, err = f.a.Reconcile(f.ctx, raw)
		case "finalize":
			answer, err = f.a.Finalize(f.ctx, raw)
		case "reconcile_complete":
			answer, err = f.a.CompleteReconcile(f.ctx, raw)
		default:
			t.Fatal("unexpected recovery message", e.Type)
		}
		if err != nil {
			t.Fatalf("Agent %s: %v", e.Type, err)
		}
		transportWrite(t, f.ctx, p, answer)
		if e.Type == "reconcile_complete" {
			var round string
			transportEventually(t, f.ctx, "Manager did not commit current completed recovery", func() bool {
				state, err := f.m.ReadRuntimeConnectionState(f.ctx, b)
				if err != nil {
					t.Fatal(err)
				}
				round = state.Recovery.RoundID
				return !state.Frozen && state.Recovery.Status == "completed" && round != ""
			})
			return round
		}
	}
	t.Fatal("recovery failed to converge within bounded message budget")
	return ""
}

func (f *transportPair) apply(t *testing.T) operations.Operation {
	t.Helper()
	config := map[string]any{"files": []any{map[string]any{"path": "compose.yaml", "content_b64": base64.StdEncoding.EncodeToString([]byte("services: {}\n"))}}, "compose_files": []string{"compose.yaml"}, "interpolation_env_files": []string{}, "interpolation_env": map[string]string{}, "profiles": []string{}}
	created := f.service(t, "POST", "/api/v1/nodes/"+f.node+"/stacks", map[string]any{"project_name": "transport_project", "config": config}, "")
	accepted := f.service(t, "POST", "/api/v1/stacks/"+created["stack_id"].(string)+"/apply", map[string]any{"version_id": created["version_id"]}, "")
	op, err := f.m.Get(f.ctx, accepted["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.h.WakeAll()
	return op
}

func (f *transportPair) prepared(t *testing.T, p *agentwire.Peer, op operations.Operation) ledger.Record {
	t.Helper()
	raw, e := transportRead(t, f.ctx, p, "prepare")
	var payload struct {
		Operation string `json:"operation_id"`
		Version   string `json:"version_id"`
		Digest    string `json:"input_digest"`
		Payload   string `json:"payload_b64"`
	}
	if err := json.Unmarshal(e.Body, &payload); err != nil {
		t.Fatal(err)
	}
	wantPayload, err := f.m.ReadPayload(f.ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Operation != op.OperationID || payload.Version != op.VersionID || payload.Digest != op.Digest || payload.Payload != base64.StdEncoding.EncodeToString(wantPayload) {
		t.Fatal("prepare changed fixed Apply input")
	}
	intents, err := f.m.Intents(f.ctx, op.OperationID)
	if err != nil || len(intents) != 1 || intents[0].Kind != "prepare" || intents[0].State != "pending" {
		t.Fatalf("socket write became business confirmation: %+v %v", intents, err)
	}
	receipt, err := f.a.ReceivePrepare(f.ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	r := receipt.Record
	if r.Phase != ledger.PhasePreparing || r.Digest != op.Digest {
		t.Fatalf("unexpected preparation record %+v", r)
	}
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	proof := ledger.PreparationEvidence{OperationID: r.OperationID, InputFingerprint: r.LocalFingerprint, WorkspaceID: "workspaces/" + r.StackID + "/" + r.VersionID + "/package", ManifestFingerprint: f.profile.Fingerprint, ProfileFingerprint: f.profile.Fingerprint, EngineID: f.profile.EngineID, DockerVersion: f.profile.DockerVersion, ComposeVersion: f.profile.ComposeVersion, Pre: ledger.Baseline{CollectedAt: stamp, Resources: []ledger.ResourceRecord{}}, Expected: []ledger.ExpectedResource{}}
	r, err = f.a.CommitPrepared(f.ctx, r.OperationID, proof)
	if err != nil {
		t.Fatal(err)
	}
	transportWrite(t, f.ctx, p, transportMessage(t, "ready", p.SessionID(), e.MessageID, map[string]any{"operation_id": r.OperationID, "input_digest": r.Digest, "precheck_result": map[string]any{"ok": true, "checked_at": stamp, "compose_version": f.profile.ComposeVersion, "workspace_ref": proof.WorkspaceID + "/"}}))
	transportEventually(t, f.ctx, "ready was not committed", func() bool {
		got, err := f.m.Get(f.ctx, r.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State == "ready"
	})
	return r
}

func assertTransportClaims(t *testing.T, f *transportPair, op operations.Operation, held bool) {
	t.Helper()
	manager, err := f.m.Claims(f.ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := f.a.Claims(f.ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := json.Marshal([]string{op.EndpointID, op.ProjectName})
	if held {
		if !reflect.DeepEqual(manager, []operations.Claim{{Kind: "project", ResourceID: string(project)}, {Kind: "stack", ResourceID: op.StackID}}) || !reflect.DeepEqual(agent, []ledger.Claim{{Kind: "project", ResourceID: string(project)}, {Kind: "stack", ResourceID: op.StackID}}) {
			t.Fatalf("claims changed: Manager=%+v Agent=%+v", manager, agent)
		}
	} else if len(manager) != 0 || len(agent) != 0 {
		t.Fatalf("claims retained: Manager=%+v Agent=%+v", manager, agent)
	}
}

func TestTLSFormalApplyPermitEventACKFinalizeAndReconnect(t *testing.T) {
	f := newTransportPair(t)
	p, b := f.dial(t)
	firstRound := f.recover(t, p, b)
	op := f.apply(t)
	r := f.prepared(t, p, op)
	assertTransportClaims(t, f, op, true)
	nonce := id(t)
	request := transportMessage(t, "request_start", b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": nonce})
	requestEnvelope := transportEnvelope(t, request)
	starts := 0
	gate, err := f.a.NewGate(f.ctx, startgate.Request{Session: b.SessionID, Operation: op.OperationID, Digest: op.Digest, Nonce: nonce, MessageID: requestEnvelope.MessageID}, time.Now, func(startgate.Permit) { starts++ })
	if err != nil {
		t.Fatal(err)
	}
	transportWrite(t, f.ctx, p, request)
	_, permitEnvelope := transportRead(t, f.ctx, p, "authorize_start")
	var permitBody struct {
		Operation string `json:"operation_id"`
		Digest    string `json:"input_digest"`
		Nonce     string `json:"start_nonce"`
		Permit    string `json:"permit_id"`
		Revision  uint64 `json:"auth_rev"`
	}
	if err := json.Unmarshal(permitEnvelope.Body, &permitBody); err != nil {
		t.Fatal(err)
	}
	if permitEnvelope.SessionID == nil || *permitEnvelope.SessionID != b.SessionID || permitEnvelope.ReplyTo == nil || *permitEnvelope.ReplyTo != requestEnvelope.MessageID || permitBody.Operation != op.OperationID || permitBody.Digest != op.Digest || permitBody.Nonce != nonce || permitBody.Revision != 1 || permitBody.Permit == "" {
		t.Fatal("permit did not bind the current request and current grant")
	}
	transportWrite(t, f.ctx, p, request)
	_, repeatedPermit := transportRead(t, f.ctx, p, "authorize_start")
	if !bytes.Equal(permitEnvelope.Body, repeatedPermit.Body) || repeatedPermit.ReplyTo == nil || *repeatedPermit.ReplyTo != requestEnvelope.MessageID {
		t.Fatal("request replay changed the committed permit")
	}
	permit := startgate.Permit{Session: b.SessionID, Operation: permitBody.Operation, Digest: permitBody.Digest, Nonce: permitBody.Nonce, PermitID: permitBody.Permit, AuthRev: permitBody.Revision, ReplyTo: *permitEnvelope.ReplyTo}
	if err := gate.Authorize(permit); err != nil {
		t.Fatal(err)
	}
	if err := gate.Authorize(permit); err != nil || starts != 1 {
		t.Fatalf("permit replay started %d times: %v", starts, err)
	}
	started, err := f.a.ReadEvent(f.ctx, r.OperationID, 1)
	if err != nil || started.Kind != "started" {
		t.Fatal("durable started", err)
	}
	// The controlled launcher deliberately never attempts a process. This is
	// an actual Agent5 no-spawn result after the durable start commitment.
	result, err := f.a.RecordStartFailure(f.ctx, r.OperationID, ledger.Failure{BootID: f.a.BootID(), Code: "spawn_failed"})
	if err != nil || result.Seq != 2 {
		t.Fatal("durable no-spawn result", result.Seq, err)
	}
	var noSpawn struct {
		Outcome    string `json:"outcome"`
		NotStarted bool   `json:"not_started"`
		Ownership  string `json:"ownership_status"`
		Error      struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(result.Body, &noSpawn); err != nil || noSpawn.Outcome != "not_started" || !noSpawn.NotStarted || noSpawn.Ownership != "not_checked" || noSpawn.Error.Code != "spawn_failed" {
		t.Fatalf("wrong no-spawn fact: %s %v", result.Body, err)
	}
	for _, ev := range []ledger.Event{started, result} {
		raw := transportMessage(t, ev.Kind, b.SessionID, "", json.RawMessage(ev.Body))
		transportWrite(t, f.ctx, p, raw)
		ackRaw, ack := transportRead(t, f.ctx, p, "ack")
		var ackBody struct {
			Operation string `json:"operation_id"`
			Seq       uint64 `json:"event_seq"`
		}
		if err := json.Unmarshal(ack.Body, &ackBody); err != nil {
			t.Fatal(err)
		}
		if ack.ReplyTo == nil || *ack.ReplyTo != transportEnvelope(t, raw).MessageID || ackBody.Operation != op.OperationID || ackBody.Seq != ev.Seq {
			t.Fatalf("incorrect durable ACK: %s", ackRaw)
		}
		stored, err := f.m.ReadEvent(f.ctx, op.OperationID, ev.Seq)
		if err != nil || !bytes.Equal(stored.Body, ev.Body) {
			t.Fatal("ACK preceded persistence of exact event body", err)
		}
		if err := f.a.AcknowledgeEvent(f.ctx, op.OperationID, op.Digest, ev.Seq, b.SessionID); err != nil {
			t.Fatal(err)
		}
		assertTransportClaims(t, f, op, true)
	}
	if pending, err := f.a.PendingEvents(f.ctx, op.OperationID, b.SessionID); err != nil || len(pending) != 0 {
		t.Fatal("ACKs did not durably acknowledge current-session events", len(pending), err)
	}
	before, err := f.m.Get(f.ctx, op.OperationID)
	if err != nil || before.State != "failed" || before.ReleaseStatus != "pending" || before.LatestResultSeq != 2 || before.MaxEventSeq != 2 || before.PermitID != permit.PermitID {
		t.Fatalf("wrong result projection: %+v %v", before, err)
	}
	finalize, finalEnvelope := transportRead(t, f.ctx, p, "finalize")
	var finalBody map[string]any
	if err := json.Unmarshal(finalEnvelope.Body, &finalBody); err != nil {
		t.Fatal(err)
	}
	if finalBody["result_event_seq"] != float64(2) || finalBody["terminal_state"] != "failed" || finalBody["operation_id"] != op.OperationID || finalBody["input_digest"] != op.Digest {
		t.Fatal("finalize changed exact result binding")
	}
	finalized, err := f.a.Finalize(f.ctx, finalize)
	if err != nil {
		t.Fatal(err)
	}
	transportWrite(t, f.ctx, p, finalized)
	transportEventually(t, f.ctx, "Manager did not persist finalized", func() bool {
		got, err := f.m.Get(f.ctx, op.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		return got.ReleaseStatus == "released"
	})
	assertTransportClaims(t, f, op, false)
	p.Close()
	p2, b2 := f.dial(t)
	if b2.SessionID == b.SessionID || f.recover(t, p2, b2) == firstRound {
		t.Fatal("replacement reused session or recovery round")
	}
	assertTransportClaims(t, f, op, false)
	if starts != 1 {
		t.Fatal("recovery repeated start", starts)
	}
	for seq, want := range map[uint64]ledger.Event{1: started, 2: result} {
		got, err := f.m.ReadEvent(f.ctx, op.OperationID, seq)
		if err != nil || !bytes.Equal(got.Body, want.Body) {
			t.Fatal("recovery changed immutable event", seq, err)
		}
	}
}

func TestTLSPermissionRevokedAfterReadyDeniesCurrentNonce(t *testing.T) {
	f := newTransportPair(t)
	p, b := f.dial(t)
	f.recover(t, p, b)
	op := f.apply(t)
	f.prepared(t, p, op)
	f.grant(t, []string{"node.read", "config.read", "config.write", "operation.read", "operation.cancel"}, 1)
	nonce := id(t)
	request := transportMessage(t, "request_start", b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": nonce})
	transportWrite(t, f.ctx, p, request)
	_, denial := transportRead(t, f.ctx, p, "deny_start")
	var body map[string]any
	if err := json.Unmarshal(denial.Body, &body); err != nil {
		t.Fatal(err)
	}
	if denial.ReplyTo == nil || *denial.ReplyTo != transportEnvelope(t, request).MessageID || body["operation_id"] != op.OperationID || body["start_nonce"] != nonce || body["code"] != "permission_denied" {
		t.Fatal("denial lost request identity or reason")
	}
	got, err := f.m.Get(f.ctx, op.OperationID)
	if err != nil || got.PermitID != "" || got.State != "ready" || got.LatestResultSeq != 0 {
		t.Fatalf("revoked grant obtained permit: %+v %v", got, err)
	}
	if _, err := f.a.ReadEvent(f.ctx, op.OperationID, 1); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatal("denial invented a start event", err)
	}
	assertTransportClaims(t, f, op, true)
}
