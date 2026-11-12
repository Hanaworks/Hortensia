package operations

import (
	"context"
	"docker-manager/internal/protocol"
	"encoding/json"
	"time"
)

// Delivery contains an already durable, current-session request. A successful
// socket write never acknowledges its business effect. Raw contains secrets.
type Delivery struct {
	MessageID   string
	Kind        string
	OperationID string
	Generation  uint64
	Raw         []byte
}

func CreateRuntimeService(ctx context.Context, path, managerID string, keys Keys, b Bootstrap) (*Store, error) {
	if !usernamePattern.MatchString(b.Username) {
		return nil, serviceError(400, "invalid_field")
	}
	hash, e := hashPassword(ctx, b.Password)
	if e != nil {
		return nil, e
	}
	return createVersion(ctx, path, managerID, keys, 5, serviceBootstrap{b.Username, hash})
}
func OpenRuntimeService(ctx context.Context, path string, expected Identity, keys Keys) (*Store, error) {
	return openVersion(ctx, path, expected, keys, 5)
}

// BindAgentHello is the only runtime hello-to-binding adapter. Credential
// ownership is checked again by the existing atomic binding transaction.
func (s *Store) BindAgentHello(ctx context.Context, a *AuthenticatedNode, raw []byte) (Binding, []byte, error) {
	if s.version != 5 {
		return Binding{}, nil, ErrSchema
	}
	if a == nil || a.store != s {
		return Binding{}, nil, serviceError(401, "invalid_credentials")
	}
	env, e := s.validator.Validate(raw)
	if e != nil || env.Type != "hello" {
		return Binding{}, nil, ErrInput
	}
	var body struct {
		Node         string          `json:"node_id"`
		Ledger       string          `json:"ledger_id"`
		Boot         string          `json:"boot_id"`
		Profile      json.RawMessage `json:"execution_profile"`
		Capabilities []string        `json:"capabilities"`
	}
	if json.Unmarshal(env.Body, &body) != nil {
		return Binding{}, nil, ErrInput
	}
	if body.Node != a.node || body.Ledger != a.ledger {
		return Binding{}, nil, ErrIdentity
	}
	session, e := protocol.NewID()
	if e != nil {
		return Binding{}, nil, ErrStorage
	}
	welcome, e := protocol.NewMessage("welcome", session, env.MessageID, map[string]any{"node_id": body.Node, "session_id": session, "reconcile_required": true})
	if e != nil {
		return Binding{}, nil, ErrStorage
	}
	if e = s.BindAuthenticatedNodeSession(ctx, a, session, body.Boot, body.Profile, body.Capabilities); e != nil {
		return Binding{}, nil, e
	}
	return Binding{body.Node, body.Ledger, session}, welcome, nil
}

// Delivery retry delays are persisted when a request is taken, including when
// the writer disappears before reporting a failure.
func deliveryDelay(attempt uint64) time.Duration {
	if attempt >= 6 {
		return 30 * time.Second
	}
	return time.Second * time.Duration(uint64(1)<<(attempt-1))
}
