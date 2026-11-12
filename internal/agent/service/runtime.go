package service

import (
	"context"
	"crypto/sha256"
	"docker-manager/internal/agent/compose"
	"docker-manager/internal/agent/executor"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agent/workspace"
	"docker-manager/internal/agentwire"
	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
	"encoding/base64"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type incoming struct {
	raw []byte
	env protocol.Envelope
}
type ackRef struct {
	op, digest string
	seq        uint64
}
type session struct {
	peer   *agentwire.Peer
	id     string
	ctx    context.Context
	cancel context.CancelFunc
	in     chan incoming
	out    chan []byte
	err    chan error
	mu     sync.Mutex
	sent   map[string]ackRef
	last   map[string]time.Time
	config chan struct{}
	large  chan struct{}
}
type operation struct {
	prepared    compose.Prepared
	command     executor.Command
	gate        *startgate.Gate
	process     *executor.Process
	startErr    error
	supervised  bool
	collecting  bool
	request     startgate.Request
	lastRequest time.Time
	resolved    chan struct{}
	resolveOnce sync.Once
}
type runtimeService struct {
	c             Config
	s             *ledger.Store
	engine        *compose.Engine
	capable       bool
	readable      bool
	ctx           context.Context
	stop          context.CancelFunc
	mu            sync.Mutex
	ops           map[string]*operation
	current       *session
	work          chan func()
	wg            sync.WaitGroup
	pages         *pageCache
	queued        map[string]bool
	recoveryError error
}

func Serve(ctx context.Context, c Config) error {
	ctx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	id, k, s, lock, e := openIdentity(ctx, c)
	if e != nil {
		return e
	}
	defer lock.Close()
	defer clearKeys(&k)
	defer s.Close()
	if id.CredentialState != "active" {
		return ErrIdentity
	}
	p, e := s.RuntimeProfile(ctx)
	if e != nil {
		return e
	}
	eng, e := compose.New(compose.Profile{DockerCLI: c.DockerCLI, DockerSocket: c.DockerSocket, Runtime: p, ExecutionTimeout: time.Duration(c.ExecutionTimeoutSeconds) * time.Second})
	if e != nil {
		return ErrConfig
	}
	probe, pe := eng.Probe(ctx)
	capable := false
	readable := false
	if pe == nil {
		raw, _ := json.Marshal([]any{c.DockerCLI, c.DockerSocket, c.ExecutionTimeoutSeconds, probe.DockerVersion, probe.ComposeVersion})
		sum := sha256.Sum256(raw)
		probe.Fingerprint = base64.RawURLEncoding.EncodeToString(sum[:])
		if p.EngineID != "" && probe != p {
			probe.PolicyRevision = p.PolicyRevision + 1
		}
		if probe.EngineID == p.EngineID && probe.Fingerprint == p.Fingerprint {
			probe.PolicyRevision = p.PolicyRevision
		}
		if e = s.BindRuntimeProfile(ctx, probe); e == nil {
			p = probe
			readable = true
			capable = runtime.GOOS == "linux" && compose.IsVerifiedRuntime(p.DockerVersion, p.ComposeVersion)
		}
	}
	eng, e = compose.New(compose.Profile{DockerCLI: c.DockerCLI, DockerSocket: c.DockerSocket, Runtime: p, ExecutionTimeout: time.Duration(c.ExecutionTimeoutSeconds) * time.Second})
	if e != nil {
		return e
	}
	life, stop := context.WithCancel(context.Background())
	defer stop()
	r := &runtimeService{c: c, s: s, engine: eng, capable: capable, readable: readable, ctx: life, stop: stop, ops: map[string]*operation{}, queued: map[string]bool{}, work: make(chan func(), 32), pages: newPageCache(time.Now)}
	// One bounded pass before hello; transient storage failures retain the
	// exact process fact and are retried independently of the connection.
	if err := r.recoverPendingOutcomes(); err != nil && !errors.Is(err, ledger.ErrStorage) {
		return err
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := r.runOutcomeRecovery(); err != nil {
			r.mu.Lock()
			r.recoveryError = err
			r.mu.Unlock()
			cancelServe()
		}
	}()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			select {
			case <-life.Done():
				return
			case f := <-r.work:
				if life.Err() == nil {
					f()
				}
			}
		}
	}()
	defer func() {
		stop()
		r.mu.Lock()
		type activeProcess struct {
			gate    *startgate.Gate
			process *executor.Process
		}
		active := make([]activeProcess, 0, len(r.ops))
		for _, o := range r.ops {
			active = append(active, activeProcess{o.gate, o.process})
		}
		r.mu.Unlock()
		for _, o := range active {
			if o.gate != nil {
				o.gate.Invalidate()
			}
			if o.process != nil {
				o.process.Interrupt("agent_stopping")
			}
		}
		r.wg.Wait()
	}()
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return r.recoveryFailure()
		}
		caps := []string{}
		if capable {
			caps = []string{"compose.apply.v1", "config.bundle.v1"}
		}
		raw, hid, e := hello(s, p, caps)
		if e != nil {
			return e
		}
		ca, e := rootCAs(c)
		if e != nil {
			return e
		}
		started := time.Now()
		peer, welcome, e := agentwire.Dial(ctx, agentwire.ClientConfig{URL: connectURL(c), Token: id.ActiveSecret, RootCAs: ca, Hello: raw})
		if errors.Is(e, agentwire.ErrUnauthorized) {
			return ErrUnauthorized
		}
		if e == nil {
			if e = s.BindManagerSession(ctx, welcome, hid); e != nil {
				peer.Close()
				return e
			}
			e = r.runSession(ctx, peer)
			if ctx.Err() != nil {
				return r.recoveryFailure()
			}
		}
		if time.Since(started) > 30*time.Second {
			delay = time.Second
		}
		jitter := time.Duration(time.Now().UnixNano() % int64(delay/5+1))
		timer := time.NewTimer(delay + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return r.recoveryFailure()
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}
func connectURL(c Config) string { return "wss" + c.ManagerOrigin[len("https"):] + "/agent/v1/connect" }
func (r *runtimeService) invalidate(ss *session) {
	ss.cancel()
	r.mu.Lock()
	var gates []*startgate.Gate
	if r.current == ss {
		r.current = nil
		for _, o := range r.ops {
			if o.gate != nil {
				gates = append(gates, o.gate)
			}
		}
	}
	r.mu.Unlock()
	for _, g := range gates {
		g.Invalidate()
	}
}
func (r *runtimeService) runSession(parent context.Context, peer *agentwire.Peer) error {
	ctx, cancel := context.WithCancel(parent)
	ss := &session{peer: peer, id: peer.SessionID(), ctx: ctx, cancel: cancel, in: make(chan incoming, 32), out: make(chan []byte, 32), err: make(chan error, 2), sent: map[string]ackRef{}, last: map[string]time.Time{}, config: make(chan struct{}, 1), large: make(chan struct{}, 1)}
	r.mu.Lock()
	r.current = ss
	r.mu.Unlock()
	var network sync.WaitGroup
	network.Add(2)
	go func() {
		defer network.Done()
		for {
			raw, env, e := peer.Read(ctx)
			if e != nil {
				r.invalidate(ss)
				select {
				case ss.err <- e:
				default:
				}
				return
			}
			if env.Type == "prepare" {
				select {
				case ss.config <- struct{}{}:
				default:
					r.invalidate(ss)
					return
				}
			}
			select {
			case ss.in <- incoming{raw, env}:
			default:
				r.invalidate(ss)
				select {
				case ss.err <- ErrProtocol:
				default:
				}
				return
			}
		}
	}()
	go func() {
		defer network.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case raw := <-ss.out:
				err := peer.Write(ctx, raw)
				if len(raw) > 64<<10 && ss.large != nil {
					<-ss.large
				}
				if e := err; e != nil {
					r.invalidate(ss)
					select {
					case ss.err <- e:
					default:
					}
					return
				}
			}
		}
	}()
	defer func() {
		r.invalidate(ss)
		peer.Close()
		network.Wait()
		_ = r.s.DisconnectManagerSession(context.Background(), peer.SessionID())
	}()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-ss.err:
			return e
		case m := <-ss.in:
			e := r.dispatch(ss, m)
			if m.env.Type == "prepare" {
				<-ss.config
			}
			if e != nil {
				return e
			}
		case <-tick.C:
			if e := r.sendEvents(ss); e != nil {
				return e
			}
		}
	}
}
func (ss *session) send(raw []byte) error {
	if ss.ctx.Err() != nil {
		return ErrTransport
	}
	large := len(raw) > 64<<10 && ss.large != nil
	if large {
		select {
		case ss.large <- struct{}{}:
		case <-ss.ctx.Done():
			return ErrTransport
		}
	}
	select {
	case ss.out <- raw:
		return nil
	default:
		if large {
			<-ss.large
		}
		ss.cancel()
		return ErrTransport
	}
}
func (r *runtimeService) submit(ss *session, f func()) error {
	select {
	case r.work <- f:
		return nil
	default:
		return ErrRuntime
	}
}
func (r *runtimeService) dispatch(ss *session, m incoming) error {
	e := m.env
	var b struct {
		Operation string `json:"operation_id"`
		Digest    string `json:"input_digest"`
		Seq       uint64 `json:"event_seq"`
		Nonce     string `json:"start_nonce"`
		PermitID  string `json:"permit_id"`
		AuthRev   uint64 `json:"auth_rev"`
		Code      string `json:"code"`
	}
	if json.Unmarshal(e.Body, &b) != nil {
		return ErrProtocol
	}
	switch e.Type {
	case "reconcile":
		raw, err := r.s.Reconcile(ss.ctx, m.raw)
		if err != nil {
			return err
		}
		return ss.send(raw)
	case "reconcile_complete":
		raw, err := r.s.CompleteReconcile(ss.ctx, m.raw)
		if err != nil {
			return err
		}
		return ss.send(raw)
	case "ack":
		if e.ReplyTo == nil {
			return ErrProtocol
		}
		ss.mu.Lock()
		ref, ok := ss.sent[*e.ReplyTo]
		ss.mu.Unlock()
		if !ok || ref.op != b.Operation || ref.seq != b.Seq {
			return ErrProtocol
		}
		if err := r.s.AcknowledgeEvent(ss.ctx, ref.op, ref.digest, ref.seq, ss.id); err != nil {
			return err
		}
		return nil
	case "prepare":
		if !r.capable {
			return ErrProtocol
		}
		receipt, err := r.s.ReceivePrepare(ss.ctx, m.raw)
		if err != nil {
			return err
		}
		switch receipt.Disposition {
		case "new", "preparing":
			r.mu.Lock()
			if r.queued[b.Operation] {
				r.mu.Unlock()
				return nil
			}
			r.queued[b.Operation] = true
			r.mu.Unlock()
			return r.submit(ss, func() {
				defer func() { r.mu.Lock(); delete(r.queued, b.Operation); r.mu.Unlock() }()
				r.prepare(ss, e.MessageID, receipt.Record)
				r.waitStartWindow(ss, b.Operation)
			})
		case "ready":
			r.mu.Lock()
			o := r.ops[b.Operation]
			var prepared compose.Prepared
			if o != nil {
				prepared = o.prepared
			}
			r.mu.Unlock()
			if prepared.Package.OperationID != "" {
				return r.sendReady(ss, e.MessageID, receipt.Record, prepared)
			}
			return nil
		default:
			return nil
		}
	case "authorize_start":
		r.mu.Lock()
		o := r.ops[b.Operation]
		var gate *startgate.Gate
		if o != nil {
			gate = o.gate
		}
		r.mu.Unlock()
		if gate == nil || e.ReplyTo == nil {
			return ErrProtocol
		}
		err := gate.Authorize(startgate.Permit{Session: ss.id, Operation: b.Operation, Digest: b.Digest, Nonce: b.Nonce, PermitID: b.PermitID, AuthRev: b.AuthRev, ReplyTo: *e.ReplyTo})
		r.afterStart(b.Operation, o)
		if state := gate.State(); state != startgate.Waiting && state != startgate.Committing {
			r.resolveStart(b.Operation)
		}
		if err != nil {
			_, _ = r.s.RecordStartFailure(r.ctx, b.Operation, ledger.Failure{Code: "start_not_created", BootID: r.s.BootID()})
		}
		return nil
	case "deny_start":
		r.mu.Lock()
		o := r.ops[b.Operation]
		var gate *startgate.Gate
		if o != nil {
			gate = o.gate
		}
		r.mu.Unlock()
		if gate == nil || e.ReplyTo == nil {
			return ErrProtocol
		}
		if err := gate.Deny(startgate.Denial{Session: ss.id, Operation: b.Operation, Nonce: b.Nonce, ReplyTo: *e.ReplyTo, Code: b.Code}); err != nil {
			return err
		}
		r.resolveStart(b.Operation)
		_, err := r.s.RecordPreparationFailure(r.ctx, b.Operation, ledger.Failure{Code: "start_denied", BootID: r.s.BootID()})
		return err
	case "cancel":
		decision, err := r.s.CancelExecution(ss.ctx, m.raw)
		if err != nil {
			return err
		}
		r.resolveStart(b.Operation)
		if decision.Disposition == "interrupt_required" {
			r.mu.Lock()
			if o := r.ops[b.Operation]; o != nil && o.process != nil {
				o.process.Interrupt("canceled")
			}
			r.mu.Unlock()
		}
		return nil
	case "finalize":
		raw, err := r.s.Finalize(ss.ctx, m.raw)
		if err != nil {
			return err
		}
		r.mu.Lock()
		delete(r.ops, b.Operation)
		r.mu.Unlock()
		return ss.send(raw)
	case "observe":
		return r.submit(ss, func() { r.observe(ss, m) })
	case "protocol_error":
		return ErrProtocol
	}
	return ErrProtocol
}
func (r *runtimeService) prepare(ss *session, reply string, record ledger.Record) {
	if ss.ctx.Err() != nil || record.BootID != r.s.BootID() {
		return
	}
	fail := func(code string) {
		_, _ = r.s.RecordPreparationFailure(r.ctx, record.OperationID, ledger.Failure{Code: code, BootID: r.s.BootID()})
	}
	payload, e := r.s.ReadExecutionInput(r.ctx, record.OperationID)
	if e != nil {
		return
	}
	defer clear(payload)
	p, e := workspace.Materialize(r.ctx, r.c.StateDir, workspace.Input{OperationID: record.OperationID, InputFingerprint: record.LocalFingerprint, Payload: payload})
	if e != nil {
		fail("workspace_invalid")
		return
	}
	binding, e := r.s.ReadProjectBinding(r.ctx, record.EndpointID, record.ProjectName)
	if e != nil && !errors.Is(e, ledger.ErrNotFound) {
		fail("ownership_unavailable")
		return
	}
	prepared, e := r.engine.Prepare(r.ctx, p, binding)
	if e != nil {
		fail("preparation_failed")
		return
	}
	if _, e = r.s.CommitPrepared(r.ctx, record.OperationID, prepared.Evidence); e != nil {
		return
	}
	// Use the authenticated canonical preparation, including the Store's
	// ownership revision/classification, before any request for permission.
	prepared.Evidence, e = r.s.ReadPreparationEvidence(r.ctx, record.OperationID)
	if e != nil {
		return
	}
	o := &operation{prepared: prepared, resolved: make(chan struct{})}
	r.mu.Lock()
	if _, exists := r.ops[record.OperationID]; exists {
		r.mu.Unlock()
		return
	}
	r.ops[record.OperationID] = o
	r.mu.Unlock()
	if r.sendReady(ss, reply, record, prepared) != nil {
		return
	}
	if e = r.engine.VerifyBeforeStart(r.ctx, prepared); e != nil {
		fail("prestart_failed")
		return
	}
	command, e := r.engine.Invocation(p)
	if e != nil {
		fail("prestart_failed")
		return
	}
	if ss.ctx.Err() != nil {
		return
	}
	nonce, e := protocol.NewID()
	if e != nil {
		return
	}
	mid, e := protocol.NewID()
	if e != nil {
		return
	}
	request := startgate.Request{Session: ss.id, Operation: record.OperationID, Digest: record.Digest, Nonce: nonce, MessageID: mid}
	o.command = command
	g, e := r.s.NewGate(r.ctx, request, time.Now, func(startgate.Permit) {
		if ss.ctx.Err() != nil {
			r.mu.Lock()
			o.startErr = ErrTransport
			r.mu.Unlock()
			return
		}
		process, err := executor.Start(r.ctx, command)
		r.mu.Lock()
		o.process = process
		o.startErr = err
		r.mu.Unlock()
	})
	if e != nil {
		return
	}
	r.mu.Lock()
	o.gate = g
	o.request = request
	o.lastRequest = time.Now()
	r.mu.Unlock()
	if ss.ctx.Err() != nil {
		g.Invalidate()
		return
	}
	raw, e := wireMessage("request_start", request.Session, "", mid, map[string]any{"operation_id": record.OperationID, "input_digest": record.Digest, "start_nonce": nonce})
	if e == nil {
		if ss.send(raw) != nil {
			g.Invalidate()
		}
	}
}
func (r *runtimeService) sendReady(ss *session, reply string, record ledger.Record, p compose.Prepared) error {
	raw, e := wireMessage("ready", ss.id, reply, "", map[string]any{"operation_id": record.OperationID, "input_digest": record.Digest, "precheck_result": map[string]any{"ok": true, "checked_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "compose_version": p.Evidence.ComposeVersion, "workspace_ref": p.Evidence.WorkspaceID}})
	if e != nil {
		return e
	}
	return ss.send(raw)
}
func (r *runtimeService) afterStart(id string, o *operation) {
	r.mu.Lock()
	p := o.process
	if o.supervised {
		r.mu.Unlock()
		return
	}
	if p == nil {
		startErr := o.startErr
		r.mu.Unlock()
		if startErr != nil {
			if _, err := r.s.RecordStartFailure(r.ctx, id, ledger.Failure{Code: "spawn_failed", BootID: r.s.BootID()}); err == nil {
				r.forgetCompleted(id, o)
			}
		}
		return
	}
	o.supervised = true
	o.collecting = true
	prepared := o.prepared
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			o.collecting = false
			r.mu.Unlock()
		}()
		ctx := context.Background()
		createdErr := r.s.RecordProcessCreated(ctx, id, p.PID())
		if createdErr != nil {
			p.Interrupt("agent_stopping")
		}
		fact := p.Wait()
		fact.BootID = r.s.BootID()
		if createdErr != nil {
			return
		}
		if r.s.RecordProcessExit(ctx, id, fact) != nil {
			return
		}
		var evidence ledger.ObservationEvidence
		if fact.Outcome == "normal_exit" {
			evidence, _ = r.engine.Observe(ctx, prepared)
		}
		if _, err := r.s.RecordProcessOutcome(ctx, id, fact, evidence); err == nil {
			r.forgetCompleted(id, o)
		}
	}()
}
func (r *runtimeService) forgetCompleted(id string, o *operation) {
	o.resolveOnce.Do(func() {
		if o.resolved != nil {
			close(o.resolved)
		}
	})
	r.mu.Lock()
	if r.ops[id] == o {
		o.prepared = compose.Prepared{}
		o.command = executor.Command{}
		o.process = nil
		o.startErr = nil
		o.supervised = true
	}
	r.mu.Unlock()
}
func (r *runtimeService) sendEvents(ss *session) error {
	if err := r.retryStartRequests(ss); err != nil {
		return err
	}
	if ss.large != nil && len(ss.large) != 0 {
		return nil
	}
	refs, e := r.s.PendingEventRefs(ss.ctx, ss.id, 32)
	if e != nil {
		return e
	}
	for _, ref := range refs {
		key := ref.OperationID + "/" + strconv.FormatUint(ref.Seq, 10)
		ss.mu.Lock()
		last := ss.last[key]
		ss.mu.Unlock()
		if time.Since(last) < time.Second {
			continue
		}
		event, e := r.s.ReadEvent(ss.ctx, ref.OperationID, ref.Seq)
		if e != nil {
			return e
		}
		mid, e := protocol.NewID()
		if e != nil {
			return e
		}
		raw, e := wireMessage(event.Kind, ss.id, "", mid, event.Body)
		if e != nil {
			return e
		}
		ss.mu.Lock()
		if len(ss.sent) >= 65536 {
			ss.mu.Unlock()
			return ErrProtocol
		}
		ss.sent[mid] = ackRef{event.OperationID, event.Digest, event.Seq}
		ss.last[key] = time.Now()
		ss.mu.Unlock()
		if e = ss.send(raw); e != nil {
			return e
		}
		if len(raw) > 64<<10 {
			return nil
		}
	}
	return nil
}

// A retry adds correlation IDs to the same nonce/window. It never creates a
// second Gate or advances the original ten-second deadline.
func (r *runtimeService) retryStartRequests(ss *session) error {
	type candidate struct {
		o       *operation
		gate    *startgate.Gate
		request startgate.Request
		last    time.Time
	}
	r.mu.Lock()
	items := make([]candidate, 0, len(r.ops))
	for _, o := range r.ops {
		if o.gate != nil && o.request.Session == ss.id {
			items = append(items, candidate{o, o.gate, o.request, o.lastRequest})
		}
	}
	r.mu.Unlock()
	for _, item := range items {
		if item.gate.State() != startgate.Waiting || time.Since(item.last) < time.Second {
			continue
		}
		mid, e := protocol.NewID()
		if e != nil {
			return e
		}
		request := item.request
		request.MessageID = mid
		if e = item.gate.RegisterRequest(request); e != nil {
			if errors.Is(e, startgate.ErrExpired) {
				_, _ = r.s.RecordPreparationFailure(r.ctx, request.Operation, ledger.Failure{Code: "start_window_expired", BootID: r.s.BootID()})
				r.resolveStart(request.Operation)
				continue
			}
			return e
		}
		raw, e := wireMessage("request_start", ss.id, "", mid, map[string]any{"operation_id": request.Operation, "input_digest": request.Digest, "start_nonce": request.Nonce})
		if e != nil {
			return e
		}
		r.mu.Lock()
		item.o.lastRequest = time.Now()
		r.mu.Unlock()
		if e = ss.send(raw); e != nil {
			return e
		}
	}
	return nil
}

func (r *runtimeService) resolveStart(id string) {
	r.mu.Lock()
	o := r.ops[id]
	r.mu.Unlock()
	if o != nil && o.resolved != nil {
		o.resolveOnce.Do(func() { close(o.resolved) })
	}
}
func (r *runtimeService) waitStartWindow(ss *session, id string) {
	r.mu.Lock()
	o := r.ops[id]
	r.mu.Unlock()
	if o == nil || o.gate == nil || o.resolved == nil {
		return
	}
	select {
	case <-o.resolved:
	case <-ss.ctx.Done():
	case <-r.ctx.Done():
	}
}
