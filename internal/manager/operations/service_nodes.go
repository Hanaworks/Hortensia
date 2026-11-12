package operations

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"docker-manager/internal/protocol"
	"encoding/json"
	"errors"
	"time"
)

func enrollmentStatus(ctx context.Context, q querier, id string) (string, error) {
	var expires int64
	var consumed, revoked sql.NullInt64
	if e := q.QueryRowContext(ctx, `SELECT expires_at,consumed_at,revoked_at FROM enrollments WHERE id=?`, id).Scan(&expires, &consumed, &revoked); e != nil {
		return "", serviceStorageError(e)
	}
	if revoked.Valid {
		return "revoked", nil
	}
	if consumed.Valid {
		return "consumed", nil
	}
	if time.Now().UnixMilli() >= expires {
		return "expired", nil
	}
	return "pending", nil
}
func (s *Store) EnrollNode(ctx context.Context, token string, r EnrollmentRequest) (ServiceReply, error) {
	if !validID(r.NodeID) || !validID(r.LedgerID) || !validDigest(r.CredentialSecret) {
		return ServiceReply{}, serviceError(400, "invalid_field")
	}
	if s.version != 4 && s.version != 5 || !validDigest(token) {
		return ServiceReply{}, serviceError(401, "invalid_credentials")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	defer tx.Rollback()
	if e = s.requireActive(ctx, tx); e != nil {
		return ServiceReply{}, e
	}
	var id, node, ledger string
	var expires int64
	var consumed, revoked sql.NullInt64
	var hash []byte
	e = tx.QueryRowContext(ctx, `SELECT id,node_id,ledger_id,expires_at,consumed_at,revoked_at,candidate_hash FROM enrollments WHERE token_hash=?`, secretHash(token)).Scan(&id, &node, &ledger, &expires, &consumed, &revoked, &hash)
	if errors.Is(e, sql.ErrNoRows) {
		return ServiceReply{}, serviceError(401, "invalid_credentials")
	}
	if e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	if node != r.NodeID || revoked.Valid {
		return ServiceReply{}, serviceError(401, "invalid_credentials")
	}
	candidate := secretHash(r.CredentialSecret)
	if consumed.Valid {
		if ledger != r.LedgerID || !hmac.Equal(hash, candidate) {
			return ServiceReply{}, serviceError(401, "invalid_credentials")
		}
		return serviceReply(200, map[string]any{"node_id": node, "ledger_id": ledger, "credential_generation": 1}), nil
	}
	now := time.Now().UnixMilli()
	if now >= expires {
		return ServiceReply{}, serviceError(401, "invalid_credentials")
	}
	var generation uint64
	var nodeRevoked sql.NullInt64
	if e = tx.QueryRowContext(ctx, `SELECT credential_generation,revoked_at FROM nodes WHERE id=?`, node).Scan(&generation, &nodeRevoked); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	if generation != 0 || nodeRevoked.Valid {
		return ServiceReply{}, serviceError(401, "invalid_credentials")
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO node_credentials(node_id,generation,token_hash,enabled,created_at) VALUES(?,1,?,1,?)`, node, candidate, now); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	if _, e = tx.ExecContext(ctx, `UPDATE nodes SET ledger_id=?,credential_generation=1,frozen=1 WHERE id=?`, r.LedgerID, node); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	if _, e = tx.ExecContext(ctx, `UPDATE enrollments SET ledger_id=?,candidate_hash=?,consumed_at=? WHERE id=?`, r.LedgerID, candidate, now, id); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	if e = agentAuditTx(ctx, tx, node, "node.enroll"); e != nil {
		return ServiceReply{}, e
	}
	if e = tx.Commit(); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	return serviceReply(200, map[string]any{"node_id": node, "ledger_id": r.LedgerID, "credential_generation": 1}), nil
}
func (s *Store) authenticateNodeTx(ctx context.Context, tx *sql.Tx, hash []byte) (*AuthenticatedNode, error) {
	if s.version != 4 && s.version != 5 {
		return nil, ErrSchema
	}
	if e := s.requireActive(ctx, tx); e != nil {
		return nil, e
	}
	a := &AuthenticatedNode{store: s, hash: append([]byte(nil), hash...)}
	e := tx.QueryRowContext(ctx, `SELECT n.id,n.ledger_id,c.generation FROM node_credentials c JOIN nodes n ON n.id=c.node_id WHERE c.token_hash=? AND c.enabled=1 AND n.revoked_at IS NULL AND n.credential_generation=c.generation`, hash).Scan(&a.node, &a.ledger, &a.generation)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, serviceError(401, "invalid_credentials")
	}
	if e != nil {
		return nil, serviceStorageError(e)
	}
	return a, nil
}
func (s *Store) AuthenticateNodeCredential(ctx context.Context, secret string) (*AuthenticatedNode, error) {
	if !validDigest(secret) {
		return nil, serviceError(401, "invalid_credentials")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, serviceStorageError(e)
	}
	defer tx.Rollback()
	return s.authenticateNodeTx(ctx, tx, secretHash(secret))
}
func (s *Store) RotateNodeCredential(ctx context.Context, token string, r RotationRequest) (ServiceReply, error) {
	if !validID(r.LedgerID) || !validDigest(r.CredentialSecret) {
		return ServiceReply{}, serviceError(400, "invalid_field")
	}
	if !validDigest(token) {
		return ServiceReply{}, serviceError(401, "invalid_credentials")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	defer tx.Rollback()
	a, e := s.authenticateNodeTx(ctx, tx, secretHash(token))
	if e != nil {
		return ServiceReply{}, e
	}
	if a.ledger != r.LedgerID || token == r.CredentialSecret {
		return ServiceReply{}, serviceError(401, "invalid_credentials")
	}
	if _, e = tx.ExecContext(ctx, `UPDATE node_credentials SET enabled=0 WHERE node_id=?`, a.node); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO node_credentials(node_id,generation,token_hash,enabled,created_at) VALUES(?,?,?,1,?)`, a.node, a.generation+1, secretHash(r.CredentialSecret), time.Now().UnixMilli()); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	if _, e = tx.ExecContext(ctx, `UPDATE nodes SET credential_generation=credential_generation+1,session_id='',boot_id='',frozen=1 WHERE id=?`, a.node); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	if e = agentAuditTx(ctx, tx, a.node, "credential.rotate"); e != nil {
		return ServiceReply{}, e
	}
	if e = tx.Commit(); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	return serviceReply(200, map[string]any{"node_id": a.node, "ledger_id": a.ledger, "credential_generation": a.generation + 1}), nil
}

func agentAuditTx(ctx context.Context, tx *sql.Tx, node, action string) error {
	id, e := protocol.NewID()
	if e != nil {
		return serviceStorageError(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO audit_events(id,node_id,action,target_type,target_id,created_at,outcome) VALUES(?,?,?,'node',?,?,'committed')`, id, node, action, node, time.Now().UnixMilli()); e != nil {
		return serviceStorageError(e)
	}
	return nil
}

// Binding proves credential ownership and a validated hello only. Recovery must
// independently complete before the existing completion receipt releases frozen.
func (s *Store) BindAuthenticatedNodeSession(ctx context.Context, a *AuthenticatedNode, sessionID, agentBootID string, profile json.RawMessage, capabilities []string) error {
	if a == nil || a.store != s {
		return serviceError(401, "invalid_credentials")
	}
	if !validID(sessionID) || !validID(agentBootID) {
		return ErrInput
	}
	if len(profile) == 0 || !json.Valid(profile) {
		return ErrInput
	}
	// Validate the shared execution profile in a schema-validated hello envelope.
	raw, e := json.Marshal(map[string]any{"protocol": 1, "type": "hello", "message_id": sessionID, "session_id": nil, "reply_to": nil, "body": map[string]any{"node_id": a.node, "ledger_id": a.ledger, "boot_id": agentBootID, "agent_version": "validation-only", "compose_version": "validation-only", "capabilities": capabilities, "execution_profile": json.RawMessage(profile)}})
	if e != nil {
		return ErrInput
	}
	if _, e = s.validator.Validate(raw); e != nil {
		return ErrInput
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return serviceStorageError(e)
	}
	defer tx.Rollback()
	current, e := s.authenticateNodeTx(ctx, tx, a.hash)
	if e != nil {
		return e
	}
	if current.node != a.node || current.ledger != a.ledger || current.generation != a.generation {
		return serviceError(401, "invalid_credentials")
	}
	caps, _ := json.Marshal(capabilities)
	if _, e = tx.ExecContext(ctx, `UPDATE recovery_scans SET status='abandoned' WHERE id IN (SELECT active_round FROM recovery_nodes WHERE node_id=? AND active_round<>'')`, a.node); e != nil {
		return serviceStorageError(e)
	}
	if _, e = tx.ExecContext(ctx, `UPDATE recovery_nodes SET agent_boot=?,active_round='' WHERE node_id=?`, agentBootID, a.node); e != nil {
		return serviceStorageError(e)
	}
	if _, e = tx.ExecContext(ctx, `UPDATE nodes SET session_id=?,boot_id=?,frozen=1,last_seen_at=?,execution_profile_json=?,agent_capabilities_json=? WHERE id=?`, sessionID, s.boot, time.Now().UnixMilli(), string(profile), string(caps), a.node); e != nil {
		return serviceStorageError(e)
	}
	if e = tx.Commit(); e != nil {
		return serviceStorageError(e)
	}
	return nil
}
func (s *Store) RecordNodeSeen(ctx context.Context, a *AuthenticatedNode, sessionID string) error {
	return s.changeNodeConnection(ctx, a, sessionID, false)
}

// ReceiveReady accepts only a current authenticated transport's validated ready.
func (s *Store) ReceiveReady(ctx context.Context, a *AuthenticatedNode, sessionID string, raw []byte) error {
	if a == nil || a.store != s {
		return serviceError(401, "invalid_credentials")
	}
	env, e := s.validator.Validate(raw)
	if e != nil || env.Type != "ready" || env.SessionID == nil || *env.SessionID != sessionID {
		return ErrInput
	}
	var body struct {
		Operation string `json:"operation_id"`
		Digest    string `json:"input_digest"`
	}
	if json.Unmarshal(env.Body, &body) != nil {
		return ErrInput
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return serviceStorageError(e)
	}
	defer tx.Rollback()
	current, e := s.authenticateNodeTx(ctx, tx, a.hash)
	if e != nil {
		return e
	}
	if current.node != a.node || current.ledger != a.ledger || current.generation != a.generation {
		return serviceError(401, "invalid_credentials")
	}
	if e = s.recordReadyTx(ctx, tx, Binding{a.node, a.ledger, sessionID}, body.Operation, body.Digest); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return serviceStorageError(e)
	}
	return nil
}
func (s *Store) DisconnectNodeSession(ctx context.Context, a *AuthenticatedNode, sessionID string) error {
	return s.changeNodeConnection(ctx, a, sessionID, true)
}
func (s *Store) changeNodeConnection(ctx context.Context, a *AuthenticatedNode, sessionID string, disconnect bool) error {
	if a == nil || a.store != s || !validID(sessionID) {
		return ErrInput
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return serviceStorageError(e)
	}
	defer tx.Rollback()
	if _, e = s.authenticateNodeTx(ctx, tx, a.hash); e != nil {
		return e
	}
	query := `UPDATE nodes SET last_seen_at=? WHERE id=? AND session_id=? AND boot_id=?`
	args := []any{time.Now().UnixMilli(), a.node, sessionID, s.boot}
	if disconnect {
		query = `UPDATE nodes SET session_id='',boot_id='',frozen=1 WHERE id=? AND session_id=? AND boot_id=?`
		args = args[1:]
	}
	if _, e = tx.ExecContext(ctx, query, args...); e != nil {
		return serviceStorageError(e)
	}
	if e = tx.Commit(); e != nil {
		return serviceStorageError(e)
	}
	return nil
}
