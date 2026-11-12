package service

import (
	"context"
	"docker-manager/internal/agent/compose"
	"docker-manager/internal/agent/ledger"

	"docker-manager/internal/agent/workspace"
	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This subprocess is an explicit test CLI. Production has no switch enabling it.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "--host" && os.Getenv("DM_AGENT_SERVICE_CLI_FIXTURE") != "1" {
		args := strings.Join(os.Args[1:], " ")
		if strings.Contains(args, "info --format") {
			// Test-only barrier lets a Store-opened service encounter a later
			// transient write failure without changing the production schema.
			executable, _ := os.Executable()
			barrierPath, _ := os.ReadFile(executable + ".probe-barrier")
			if barrier := string(barrierPath); barrier != "" {
				_ = os.WriteFile(barrier+".entered", []byte("probe"), 0600)
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(barrier); err == nil {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			fmt.Print(`{"ID":"fixture-engine","ServerVersion":"fixture-docker"}`)
			os.Exit(0)
		}
		if strings.Contains(args, "compose version --short") {
			fmt.Print("fixture-compose")
			os.Exit(0)
		}
	}
	if os.Getenv("DM_AGENT_SERVICE_CLI_FIXTURE") != "1" {
		return
	}
	args := strings.Join(os.Args[1:], " ")
	f, e := os.OpenFile(os.Getenv("DM_AGENT_SERVICE_CLI_RECORD"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		os.Exit(4)
	}
	fmt.Fprintln(f, args)
	f.Close()
	switch {
	case strings.Contains(args, "config --format json"):
		fmt.Print(`{"services":{"app":{"image":"alpine"}}}`)
	case strings.Contains(args, "info --format"):
		fmt.Print(`{"ID":"fixture-engine","ServerVersion":"fixture-docker"}`)
	case strings.Contains(args, "compose version --short"):
		fmt.Print("fixture-compose")
	case strings.Contains(args, "container ls"):
		if _, err := os.Stat(os.Getenv("DM_AGENT_SERVICE_CLI_POSTFILE")); err == nil {
			fmt.Println(strings.Repeat("a", 64))
			fmt.Println(strings.Repeat("b", 64))
		}
	case strings.Contains(args, "container inspect"):
		id := os.Args[len(os.Args)-1]
		number := 1
		if strings.HasPrefix(id, "b") {
			number = 2
		}
		fmt.Printf(`[{"Id":%q,"Name":"/demo-app-%d","Image":"sha256:%s","Created":"2026-11-12T00:00:00.000Z","Config":{"Labels":{"com.docker.compose.project":"demo","com.docker.compose.service":"app","com.docker.compose.container-number":"%d"}},"State":{"Status":"exited"}}]`, id, number, strings.Repeat("c", 64), number)
	case strings.Contains(args, " ls "):
	case strings.Contains(args, "up --detach --no-build"):
		fmt.Print("fixture exit")
	default:
		os.Exit(8)
	}
	os.Exit(0)
}
func testEnvelope(t *testing.T, kind, sid, reply string, body any) incoming {
	t.Helper()
	raw, e := wireMessage(kind, sid, reply, "", body)
	if e != nil {
		t.Fatal(e)
	}
	v, e := protocol.NewValidator()
	if e != nil {
		t.Fatal(e)
	}
	env, e := v.Validate(raw)
	if e != nil {
		t.Fatal(e)
	}
	return incoming{raw, env}
}
func TestSchedulerPersistsBeforeReadyAndCorrelatesEvents(t *testing.T) {
	ctx := context.Background()
	c := testConfig(t)
	id, e := Initialize(ctx, c, "10000000-0000-4000-8000-000000000001")
	if e != nil {
		t.Fatal(e)
	}
	_, k, s, lock, e := openIdentity(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	defer clearKeys(&k)
	defer s.Close()
	p, e := s.RuntimeProfile(ctx)
	if e != nil {
		t.Fatal(e)
	}
	p.EngineID = "fixture-engine"
	p.DockerVersion = "fixture-docker"
	p.ComposeVersion = "fixture-compose"
	p.Fingerprint = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if e = s.BindRuntimeProfile(ctx, p); e != nil {
		t.Fatal(e)
	}
	sid, _ := protocol.NewID()
	_, helloID, e := hello(s, p, []string{"compose.apply.v1", "config.bundle.v1"})
	if e != nil {
		t.Fatal(e)
	}
	welcome := testEnvelope(t, "welcome", sid, helloID, map[string]any{"node_id": id.NodeID, "session_id": sid, "reconcile_required": true})
	if e = s.BindManagerSession(ctx, welcome.raw, helloID); e != nil {
		t.Fatal(e)
	}
	scan := testEnvelope(t, "reconcile", sid, "", map[string]any{"scope": "pending", "scan_id": nil, "cursor": nil, "limit": 200})
	page, e := s.Reconcile(ctx, scan.raw)
	if e != nil {
		t.Fatal(e)
	}
	var scanReply struct {
		Body struct {
			ScanID string `json:"scan_id"`
		} `json:"body"`
	}
	json.Unmarshal(page, &scanReply)
	complete := testEnvelope(t, "reconcile_complete", sid, "", map[string]any{"scan_id": scanReply.Body.ScanID, "ledger_id": id.LedgerID})
	if _, e = s.CompleteReconcile(ctx, complete.raw); e != nil {
		t.Fatal(e)
	}
	exe, _ := os.Executable()
	c.DockerCLI = exe
	eng, e := compose.New(compose.Profile{DockerCLI: exe, DockerSocket: c.DockerSocket, Runtime: p})
	if e != nil {
		t.Fatal(e)
	}
	recordFile := filepath.Join(t.TempDir(), "commands")
	payload := workspace.Payload{OperationType: "stack.apply", NodeID: id.NodeID, StackID: "20000000-0000-4000-8000-000000000001", VersionID: "30000000-0000-4000-8000-000000000001", ProjectName: "demo", RequiredCapabilities: []string{"compose.apply.v1", "config.bundle.v1"}}
	payload.ExecutionProfile.EndpointID = p.EndpointID
	payload.ExecutionProfile.PolicyRevision = p.PolicyRevision
	payload.Config = workspace.Config{Files: []workspace.File{{Path: "compose.yaml", ContentB64: base64.StdEncoding.EncodeToString([]byte("services:\n  app:\n    image: alpine\n"))}}, ComposeFiles: []string{"compose.yaml"}, Env: map[string]string{"DM_AGENT_SERVICE_CLI_FIXTURE": "1", "DM_AGENT_SERVICE_CLI_RECORD": recordFile, "DM_AGENT_SERVICE_CLI_POSTFILE": recordFile + ".post"}, EnvFiles: []string{}, Profiles: []string{}}
	rawPayload, _ := json.Marshal(payload)
	op := "40000000-0000-4000-8000-000000000001"
	digest := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	prep := testEnvelope(t, "prepare", sid, "", map[string]any{"operation_id": op, "input_digest": digest, "node_id": id.NodeID, "stack_id": payload.StackID, "version_id": payload.VersionID, "payload_b64": base64.StdEncoding.EncodeToString(rawPayload)})
	receipt, e := s.ReceivePrepare(ctx, prep.raw)
	if e != nil {
		t.Fatal(e)
	}
	life, stop := context.WithCancel(ctx)
	defer stop()
	r := &runtimeService{c: c, s: s, engine: eng, capable: true, readable: true, ctx: life, stop: stop, ops: map[string]*operation{}, queued: map[string]bool{}, pages: newPageCache(time.Now)}
	ss := &session{id: sid, ctx: life, cancel: stop, out: make(chan []byte, 32), sent: map[string]ackRef{}, last: map[string]time.Time{}}
	r.prepare(ss, prep.env.MessageID, receipt.Record)
	v, _ := protocol.NewValidator()
	var request protocol.Envelope
	for i := 0; i < 2; i++ {
		select {
		case b := <-ss.out:
			env, e := v.Validate(b)
			if e != nil {
				t.Fatal(e)
			}
			if env.Type == "request_start" {
				request = env
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing ready/request")
		}
	}
	r.mu.Lock()
	o := r.ops[op]
	r.mu.Unlock()
	if o == nil || o.gate == nil || request.MessageID == "" {
		t.Fatal("not prepared")
	}
	saved, e := s.ReadPreparationEvidence(ctx, op)
	if e != nil || saved.ManifestFingerprint != o.prepared.Evidence.ManifestFingerprint {
		t.Fatal("ready lacked saved preparation")
	}
	calls, _ := os.ReadFile(recordFile)
	if strings.Contains(string(calls), "up --detach") {
		t.Fatal("up before permit")
	}
	var req struct {
		Nonce string `json:"start_nonce"`
	}
	json.Unmarshal(request.Body, &req)
	r.mu.Lock()
	o.lastRequest = time.Now().Add(-2 * time.Second)
	r.mu.Unlock()
	if e = r.retryStartRequests(ss); e != nil {
		t.Fatal(e)
	}
	retryRaw := <-ss.out
	retry, e := v.Validate(retryRaw)
	if e != nil || retry.Type != "request_start" || retry.MessageID == request.MessageID || string(retry.Body) != string(request.Body) {
		t.Fatal("retry changed nonce/window input", e)
	}
	permitID, _ := protocol.NewID()
	permit := testEnvelope(t, "authorize_start", sid, request.MessageID, map[string]any{"operation_id": op, "input_digest": digest, "start_nonce": req.Nonce, "permit_id": permitID, "auth_rev": 1})
	if e = r.dispatch(ss, permit); e != nil {
		t.Fatal(e)
	}
	r.wg.Wait()
	event, e := s.ReadEvent(ctx, op, 2)
	if e != nil {
		t.Fatal("missing durable result", e)
	}
	var result struct {
		Outcome     string          `json:"outcome"`
		Observation json.RawMessage `json:"observation"`
	}
	json.Unmarshal(event.Body, &result)
	calls, _ = os.ReadFile(recordFile)
	terminal := "failed"
	if runtime.GOOS == "linux" {
		if result.Outcome != "normal_exit" || strings.Count(string(calls), "up --detach --no-build") != 1 {
			t.Fatal("real supervised test process missing")
		}
		terminal = "succeeded"
	} else {
		if result.Outcome != "not_started" || strings.Contains(string(calls), "up --detach") {
			t.Fatal("unsupported platform spawned process")
		}
	}
	wrongID, _ := protocol.NewID()
	bad := testEnvelope(t, "ack", sid, wrongID, map[string]any{"operation_id": op, "event_seq": 2})
	if e = r.dispatch(ss, bad); e != ErrProtocol {
		t.Fatal("unknown ACK correlation accepted", e)
	}
	if event, e = s.ReadEvent(ctx, op, 2); e != nil || event.AckSession != "" {
		t.Fatal("rejected ACK changed durable event")
	}
	if e = r.sendEvents(ss); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		raw := <-ss.out
		env, e := v.Validate(raw)
		if e != nil {
			t.Fatal(e)
		}
		var b struct {
			Seq uint64 `json:"event_seq"`
		}
		json.Unmarshal(env.Body, &b)
		ack := testEnvelope(t, "ack", sid, env.MessageID, map[string]any{"operation_id": op, "event_seq": b.Seq})
		if e = r.dispatch(ss, ack); e != nil {
			t.Fatal(e)
		}
	}
	claims, e := s.Claims(ctx, op)
	if e != nil || len(claims) != 2 {
		t.Fatal("ACK released claims")
	}
	finalize := testEnvelope(t, "finalize", sid, "", map[string]any{"operation_id": op, "input_digest": digest, "result_event_seq": 2, "terminal_state": terminal})
	if e = r.dispatch(ss, finalize); e != nil {
		t.Fatal(e)
	}
	claims, e = s.Claims(ctx, op)
	if e != nil || len(claims) != 0 {
		t.Fatal("formal finalize retained claims")
	}

	// A controlled ledger process-fact fixture exercises the supplemental
	// observation service on Windows without pretending to run Docker there.
	op2 := "40000000-0000-4000-8000-000000000002"
	prep2 := testEnvelope(t, "prepare", sid, "", map[string]any{"operation_id": op2, "input_digest": digest, "node_id": id.NodeID, "stack_id": payload.StackID, "version_id": payload.VersionID, "payload_b64": base64.StdEncoding.EncodeToString(rawPayload)})
	receipt2, e := s.ReceivePrepare(ctx, prep2.raw)
	if e != nil {
		t.Fatal(e)
	}
	pkg, e := workspace.Materialize(ctx, c.StateDir, workspace.Input{OperationID: op2, InputFingerprint: receipt2.Record.LocalFingerprint, Payload: rawPayload})
	if e != nil {
		t.Fatal(e)
	}
	binding, e := s.ReadProjectBinding(ctx, p.EndpointID, payload.ProjectName)
	if e != nil {
		t.Fatal(e)
	}
	prepared, e := eng.Prepare(ctx, pkg, binding)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CommitPrepared(ctx, op2, prepared.Evidence); e != nil {
		t.Fatal(e)
	}
	nonce, _ := protocol.NewID()
	requestID, _ := protocol.NewID()
	permit2, _ := protocol.NewID()
	g, e := s.NewGate(ctx, startgate.Request{Session: sid, Operation: op2, Digest: digest, Nonce: nonce, MessageID: requestID}, time.Now, func(startgate.Permit) {})
	if e != nil {
		t.Fatal(e)
	}
	if e = g.Authorize(startgate.Permit{Session: sid, Operation: op2, Digest: digest, Nonce: nonce, PermitID: permit2, AuthRev: 1, ReplyTo: requestID}); e != nil {
		t.Fatal(e)
	}
	if e = s.RecordProcessCreated(ctx, op2, 1); e != nil {
		t.Fatal(e)
	}
	zero := 0
	fact := ledger.ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Output: []byte("fixed original output")}
	if e = s.RecordProcessExit(ctx, op2, fact); e != nil {
		t.Fatal(e)
	}
	failure, _ := json.Marshal(map[string]any{"status": "error", "collected_at": fact.FinishedAt, "items": []any{}, "error": map[string]any{"category": "unavailable", "code": "resource_query_failed", "message": "Resource query failed.", "retry_action": "query_operation"}})
	if _, e = s.RecordProcessOutcome(ctx, op2, fact, ledger.ObservationEvidence{Observation: failure}); e != nil {
		t.Fatal(e)
	}
	queryID, _ := protocol.NewID()
	if e = os.WriteFile(recordFile+".post", []byte("post"), 0600); e != nil {
		t.Fatal(e)
	}
	observe := testEnvelope(t, "observe", sid, "", map[string]any{"query_id": queryID, "operation_id": op2, "resource_kind": "container", "stack_id": payload.StackID, "cursor": nil, "limit": 1})
	qs := &session{id: sid, ctx: ctx, cancel: func() {}, out: make(chan []byte, 32)}
	r.observe(qs, observe)
	reply := <-qs.out
	if _, e = v.Validate(reply); e != nil {
		t.Fatal(e)
	}
	var firstPage struct {
		Body struct {
			Items []json.RawMessage `json:"items"`
			Next  *string           `json:"next_cursor"`
			Error json.RawMessage   `json:"error"`
		} `json:"body"`
	}
	if json.Unmarshal(reply, &firstPage) != nil || len(firstPage.Body.Items) != 1 || firstPage.Body.Next == nil || string(firstPage.Body.Error) != "null" {
		t.Fatal("missing fixed observation page", string(reply))
	}
	update, e := s.ReadEvent(ctx, op2, 3)
	if e != nil {
		t.Fatal("supplemental result missing", e)
	}
	var updated struct {
		Outcome  string `json:"outcome"`
		Output   string `json:"output_b64"`
		Finished string `json:"finished_at"`
	}
	json.Unmarshal(update.Body, &updated)
	if updated.Outcome != "normal_exit" || updated.Finished != fact.FinishedAt || updated.Output != base64.StdEncoding.EncodeToString(fact.Output) {
		t.Fatal("supplemental observation changed process fact")
	}
	before, _ := os.ReadFile(recordFile)
	r.observe(qs, observe)
	repeat := <-qs.out
	if _, e = v.Validate(repeat); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(recordFile)
	if string(before) != string(after) {
		t.Fatal("same query collected twice")
	}
	nextPage := testEnvelope(t, "observe", sid, "", map[string]any{"query_id": queryID, "operation_id": op2, "resource_kind": "container", "stack_id": payload.StackID, "cursor": *firstPage.Body.Next, "limit": 1})
	r.observe(qs, nextPage)
	lastReply := <-qs.out
	var lastPage struct {
		Body struct {
			Items []json.RawMessage `json:"items"`
			Next  *string           `json:"next_cursor"`
		} `json:"body"`
	}
	if json.Unmarshal(lastReply, &lastPage) != nil || len(lastPage.Body.Items) != 1 || lastPage.Body.Next != nil || string(lastPage.Body.Items[0]) == string(firstPage.Body.Items[0]) {
		t.Fatal("second page was not fixed snapshot")
	}
	after, _ = os.ReadFile(recordFile)
	if string(before) != string(after) {
		t.Fatal("pagination collected again")
	}
	q, e := s.BeginObservationQuery(ctx, op2, queryID, 1)
	if e != nil || q.ResultSeq != 3 || q.Disposition != "replay" {
		t.Fatal("query result association was not durable", q, e)
	}
}
