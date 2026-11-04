// Package startgate is an isolated, in-memory experiment for the Agent v1
// start-permit boundary. It does not implement a production Agent, protocol
// decoding, Compose execution, durable storage, recovery, or result reporting.
// In particular, CommitStart is an injected boundary, not proof of durability.
// Gate state never establishes a persistent not_started or execution outcome.
//
// One Gate represents one prepared operation and one start nonce. The caller
// must validate the complete wire messages against the v1 schema first, bind
// them to the authenticated connection, and call Invalidate when that connection
// closes or is replaced. A new session must reconcile; constructing another Gate
// is not permission to retry the same operation after reconnect or uncertainty.
package startgate

import (
	"errors"
	"sync"
	"time"
)

// Request contains the request_start envelope correlation and body fields.
type Request struct{ Session, Operation, Digest, Nonce, MessageID string }

// Permit contains the authorize_start correlation and all body fields. Session
// comes from the envelope; the ten-second deadline is local, not a wire field.
type Permit struct {
	Session, Operation, Digest, Nonce, PermitID string
	AuthRev                                     uint64
	ReplyTo                                     string
}

// Denial mirrors deny_start. It intentionally has no digest: v1 has none here.
type Denial struct{ Session, Operation, Nonce, ReplyTo, Code string }

// Hooks supply the boundaries exercised by this experiment. Now must advance
// monotonically; use time.Now (without stripping its monotonic reading) outside
// deterministic tests. New records the first request time using Now.
//
// CommitStart runs without Gate's mutex, so it may call Cancel or Invalidate.
// Start runs in the final serialized check/start section. It must only initiate
// work, return promptly, and must not reenter Gate; it must not wait for execution
// completion. Start's return does not establish any execution outcome.
type Hooks struct {
	Now         func() time.Time
	CommitStart func(Permit) error
	Start       func(Permit)
}

// State is local gate state, not an Agent phase or a Manager operation result.
type State string

const (
	Waiting     State = "waiting"
	Committing  State = "committing"
	Started     State = "started"
	Canceled    State = "canceled"
	Invalidated State = "invalidated"
	Denied      State = "denied"
	Expired     State = "expired"
	Uncertain   State = "uncertain"
)

var (
	ErrSession        = errors.New("session mismatch")
	ErrOperation      = errors.New("operation mismatch")
	ErrDigest         = errors.New("digest mismatch")
	ErrNonce          = errors.New("nonce mismatch")
	ErrReplyTo        = errors.New("request correlation mismatch")
	ErrPermit         = errors.New("invalid permit")
	ErrPermitConflict = errors.New("permit conflicts with committed choice")
	ErrExpired        = errors.New("permit window expired")
	ErrCanceled       = errors.New("operation canceled")
	ErrInvalidated    = errors.New("connection invalidated")
	ErrCommitFailed   = errors.New("start commit failed")
	ErrUncertain      = errors.New("start commitment uncertain")
	ErrCommitting     = errors.New("start commit in progress")
	ErrDenied         = errors.New("start denied")
	ErrAlreadyStarted = errors.New("execution already started")
	ErrRequest        = errors.New("invalid start request or hooks")
)

// Gate serializes authorization consumption for a single operation. Its zero
// value is not usable. Gate must not be copied after construction.
type Gate struct {
	mu           sync.Mutex
	request      Request
	hooks        Hooks
	deadline     time.Time
	requests     map[string]struct{}
	state        State
	chosen       *Permit
	disconnected bool
}

// New records the first request_start and starts its fixed ten-second window.
// Call it at the send boundary, not when an operation first becomes ready.
func New(r Request, h Hooks) (*Gate, error) {
	if r.Session == "" || r.Operation == "" || r.Digest == "" || r.Nonce == "" || r.MessageID == "" || h.Now == nil || h.CommitStart == nil || h.Start == nil {
		return nil, ErrRequest
	}
	return &Gate{request: r, hooks: h, deadline: h.Now().Add(10 * time.Second), requests: map[string]struct{}{r.MessageID: {}}, state: Waiting}, nil
}

// RegisterRequest records a same-session, same-nonce retransmission. It keeps
// every accepted request message ID and never moves the original deadline.
func (g *Gate) RegisterRequest(r Request) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.checkIdentity(r.Session, r.Operation, r.Nonce); err != nil {
		return err
	}
	if r.Digest != g.request.Digest {
		return ErrDigest
	}
	if r.MessageID == "" {
		return ErrRequest
	}
	if err := g.pending(); err != nil {
		return err
	}
	g.requests[r.MessageID] = struct{}{}
	return nil
}

// Authorize consumes a matching permit at most once. A replay after Start reads
// the existing choice and does not commit or start again. A concurrent replay
// during CommitStart returns ErrCommitting; it must not launch another attempt.
// Commit failure freezes this Gate, because this in-memory experiment cannot
// establish whether an external commit was durable.
func (g *Gate) Authorize(p Permit) error {
	g.mu.Lock()
	if g.disconnected {
		g.mu.Unlock()
		return ErrInvalidated
	}
	if err := g.checkIdentity(p.Session, p.Operation, p.Nonce); err != nil {
		g.mu.Unlock()
		return err
	}
	if p.Digest != g.request.Digest {
		g.mu.Unlock()
		return ErrDigest
	}
	if _, ok := g.requests[p.ReplyTo]; !ok {
		g.mu.Unlock()
		return ErrReplyTo
	}
	if p.PermitID == "" || p.AuthRev > 9007199254740991 {
		g.mu.Unlock()
		return ErrPermit
	}
	if g.chosen != nil && (p.PermitID != g.chosen.PermitID || p.AuthRev != g.chosen.AuthRev) {
		g.mu.Unlock()
		return ErrPermitConflict
	}
	if g.state == Started {
		g.mu.Unlock()
		return nil
	}
	if err := g.pending(); err != nil {
		g.mu.Unlock()
		return err
	}
	g.chosen = &p
	g.state = Committing
	g.mu.Unlock()

	commitErr := g.hooks.CommitStart(p)

	g.mu.Lock()
	defer g.mu.Unlock()
	if commitErr != nil {
		if g.state == Committing {
			g.state = Uncertain
		}
		return errors.Join(ErrCommitFailed, commitErr)
	}
	// Invalidate/Cancel may have run while CommitStart was outside the lock.
	if g.state != Committing {
		return g.stateError()
	}
	if !g.hooks.Now().Before(g.deadline) {
		g.state = Expired
		return ErrExpired
	}
	// Keep the final validity check and invocation in the same serialized region:
	// cancellation cannot win between the check and the actual Start callback.
	g.state = Started
	g.hooks.Start(p)
	return nil
}

// Cancel stops a not-yet-started gate, including during CommitStart. It never
// claims execution rollback and never overwrites Started or Uncertain.
func (g *Gate) Cancel() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.state {
	case Waiting, Committing, Canceled:
		g.state = Canceled
		return nil
	default:
		return g.stateError()
	}
}

// Invalidate permanently closes an unconsumed gate when its connection closes
// or is replaced. An already-started fact remains started; this is not stopping
// or undoing an execution. There is no method for installing a new session.
func (g *Gate) Invalidate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.disconnected = true
	if g.state == Waiting || g.state == Committing {
		g.state = Invalidated
	}
}

// Deny only changes a still-pending, correlated request. A denial arriving
// during/after commitment cannot manufacture a not-started execution result.
func (g *Gate) Deny(d Denial) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.checkIdentity(d.Session, d.Operation, d.Nonce); err != nil {
		return err
	}
	if _, ok := g.requests[d.ReplyTo]; !ok {
		return ErrReplyTo
	}
	if d.Code == "" {
		return ErrRequest
	}
	if err := g.pending(); err != nil {
		return err
	}
	g.state = Denied
	return nil
}

// State returns local gate status, without asserting durable execution facts.
func (g *Gate) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

func (g *Gate) checkIdentity(session, operation, nonce string) error {
	if session != g.request.Session {
		return ErrSession
	}
	if operation != g.request.Operation {
		return ErrOperation
	}
	if nonce != g.request.Nonce {
		return ErrNonce
	}
	return nil
}

func (g *Gate) pending() error {
	if g.disconnected {
		return ErrInvalidated
	}
	if g.state != Waiting {
		return g.stateError()
	}
	if !g.hooks.Now().Before(g.deadline) {
		g.state = Expired
		return ErrExpired
	}
	return nil
}

func (g *Gate) stateError() error {
	switch g.state {
	case Started:
		return ErrAlreadyStarted
	case Committing:
		return ErrCommitting
	case Canceled:
		return ErrCanceled
	case Invalidated:
		return ErrInvalidated
	case Denied:
		return ErrDenied
	case Expired:
		return ErrExpired
	case Uncertain:
		return ErrUncertain
	default:
		return ErrRequest
	}
}
