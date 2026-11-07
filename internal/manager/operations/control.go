package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"docker-manager/internal/protocol"
)

// Fixture registration supplies trusted identities, not authentication.
func (s *Store) CreateUserFixture(ctx context.Context, id string, enabled bool) error {
	if !validID(id) {
		return ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireActive(ctx, tx); err != nil {
		return err
	}
	var exists int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id=?", id).Scan(&exists)
	if err == nil {
		return ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,enabled,auth_rev) VALUES(?,?,1)", id, enabled); err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) SetUserEnabledFixture(ctx context.Context, id string, enabled bool) error {
	if !validID(id) {
		return ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireActive(ctx, tx); err != nil {
		return err
	}
	var old bool
	err = tx.QueryRowContext(ctx, "SELECT enabled FROM users WHERE id=?", id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return ErrStorage
	}
	if old != enabled {
		if _, err = tx.ExecContext(ctx, "UPDATE users SET enabled=?,auth_rev=auth_rev+1 WHERE id=?", enabled, id); err != nil {
			return ErrStorage
		}
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func validCapability(capability string) bool {
	switch capability {
	case "node.read", "config.read", "config.write", "stack.deploy", "operation.read", "operation.output.read", "operation.cancel":
		return true
	}
	return false
}

func (s *Store) SetGrantFixture(ctx context.Context, user, node, capability string, granted bool) error {
	if !validID(user) || !validID(node) || !validCapability(capability) {
		return ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireActive(ctx, tx); err != nil {
		return err
	}
	var exists int
	for _, entry := range []struct{ query, id string }{{"SELECT 1 FROM users WHERE id=?", user}, {"SELECT 1 FROM nodes WHERE id=?", node}} {
		err = tx.QueryRowContext(ctx, entry.query, entry.id).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return ErrStorage
		}
	}
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM grants WHERE user_id=? AND node_id=? AND capability=?", user, node, capability).Scan(&exists)
	present := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ErrStorage
	}
	if present != granted {
		if granted {
			_, err = tx.ExecContext(ctx, "INSERT INTO grants(user_id,node_id,capability) VALUES(?,?,?)", user, node, capability)
		} else {
			_, err = tx.ExecContext(ctx, "DELETE FROM grants WHERE user_id=? AND node_id=? AND capability=?", user, node, capability)
		}
		if err != nil {
			return ErrStorage
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET auth_rev=auth_rev+1 WHERE id=?", user); err != nil {
			return ErrStorage
		}
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) BindSessionFixture(ctx context.Context, b Binding) error {
	if !validID(b.NodeID) || !validID(b.LedgerID) || !validID(b.SessionID) {
		return ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireActive(ctx, tx); err != nil {
		return err
	}
	var ledger, session, boot string
	err = tx.QueryRowContext(ctx, "SELECT ledger_id,session_id,boot_id FROM nodes WHERE id=?", b.NodeID).Scan(&ledger, &session, &boot)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var recovery bool
		if err = tx.QueryRowContext(ctx, "SELECT recovery_mode FROM manager_meta WHERE singleton=1").Scan(&recovery); err != nil {
			return ErrStorage
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO nodes(id,ledger_id,session_id,boot_id,frozen) VALUES(?,?,?,?,?)", b.NodeID, b.LedgerID, b.SessionID, s.boot, recovery)
	case err != nil:
		return ErrStorage
	case ledger != b.LedgerID:
		return ErrIdentity
	case session != b.SessionID || boot != s.boot:
		_, err = tx.ExecContext(ctx, "UPDATE nodes SET session_id=?,boot_id=?,frozen=1 WHERE id=?", b.SessionID, s.boot, b.NodeID)
	}
	if err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

// allowFrozen permits evidence ingestion while a replacement session reconciles;
// it never permits an old Store or a non-current connection to mutate evidence.
func (s *Store) requireBinding(ctx context.Context, q querier, b Binding, allowFrozen bool) error {
	if !validID(b.NodeID) || !validID(b.LedgerID) || !validID(b.SessionID) {
		return ErrInput
	}
	if err := s.requireActive(ctx, q); err != nil {
		return err
	}
	var ledger, session, boot string
	var frozen bool
	err := q.QueryRowContext(ctx, "SELECT ledger_id,session_id,boot_id,frozen FROM nodes WHERE id=?", b.NodeID).Scan(&ledger, &session, &boot, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrIdentity
	}
	if err != nil {
		return ErrStorage
	}
	if ledger != b.LedgerID {
		return ErrIdentity
	}
	if session != b.SessionID || boot != s.boot || frozen && !allowFrozen {
		return ErrFrozen
	}
	return nil
}

func (s *Store) requirePermission(ctx context.Context, q querier, user, node string) (uint64, error) {
	var enabled bool
	var revision uint64
	err := q.QueryRowContext(ctx, "SELECT enabled,auth_rev FROM users WHERE id=?", user).Scan(&enabled, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrPermission
	}
	if err != nil {
		return 0, ErrStorage
	}
	if !enabled {
		return 0, ErrPermission
	}
	var count int
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM grants WHERE user_id=? AND node_id=? AND capability IN ('node.read','config.read','stack.deploy')", user, node).Scan(&count); err != nil {
		return 0, ErrStorage
	}
	if count != 3 {
		return 0, ErrPermission
	}
	return revision, nil
}

func bindingMatchesOperation(b Binding, o Operation) bool {
	return b.NodeID == o.NodeID && b.LedgerID == o.LedgerID
}
func terminalState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "canceled"
}

func (s *Store) RecordReadyFixture(ctx context.Context, b Binding, id, digest string) error {
	if !validID(id) || !validDigest(digest) {
		return ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireBinding(ctx, tx, b, false); err != nil {
		return err
	}
	op, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return err
	}
	if !bindingMatchesOperation(b, op) {
		return ErrIdentity
	}
	if op.Digest != digest || op.OwnerBoot != s.boot || op.PreparedSession != b.SessionID || op.CancelRequested || op.Blocked || op.LatestResultSeq != 0 {
		return ErrConflict
	}
	if err = s.requireOperationEvidence(ctx, tx, op); err != nil {
		return err
	}
	if op.State != "accepted" && op.State != "ready" {
		return ErrConflict
	}
	if op.State == "accepted" {
		if _, err = tx.ExecContext(ctx, "UPDATE operations SET state='ready',revision=revision+1,updated_at=? WHERE operation_id=?", time.Now().UnixMilli(), id); err != nil {
			return ErrStorage
		}
		res, err := tx.ExecContext(ctx, "UPDATE outbox SET delivery_state='confirmed' WHERE operation_id=? AND kind='prepare' AND generation=1", id)
		if err != nil {
			return ErrStorage
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return ErrEvidence
		}
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

// AuthorizeStart commits the durable decision before constructing its reply.
// The Agent's monotonic gate, not this store, owns the ten-second window.
func (s *Store) AuthorizeStart(ctx context.Context, b Binding, raw []byte) ([]byte, error) {
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "request_start" || env.SessionID == nil {
		return nil, ErrInput
	}
	if *env.SessionID != b.SessionID {
		return nil, ErrFrozen
	}
	var body struct {
		Operation string `json:"operation_id"`
		Digest    string `json:"input_digest"`
		Nonce     string `json:"start_nonce"`
	}
	if err = json.Unmarshal(env.Body, &body); err != nil {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireBinding(ctx, tx, b, false); err != nil {
		return nil, err
	}
	op, err := s.readOperation(ctx, tx, body.Operation)
	if err != nil {
		return nil, err
	}
	if !bindingMatchesOperation(b, op) {
		return nil, ErrIdentity
	}
	if body.Digest != op.Digest || op.OwnerBoot != s.boot || op.PreparedSession != b.SessionID {
		return nil, ErrConflict
	}
	if err = s.requireOperationEvidence(ctx, tx, op); err != nil {
		return nil, err
	}
	if op.Blocked || op.LatestResultSeq != 0 || terminalState(op.State) || op.State == "reconciliation_required" {
		return nil, ErrFrozen
	}
	if op.PermitID != "" {
		if op.PermitSession != b.SessionID || op.StartNonce != body.Nonce {
			return nil, ErrConflict
		}
	} else {
		if op.State != "ready" || op.CancelRequested {
			return nil, ErrConflict
		}
		op.AuthRev, err = s.requirePermission(ctx, tx, op.UserID, op.NodeID)
		if err != nil {
			return nil, err
		}
		op.PermitID, err = protocol.NewID()
		if err != nil {
			return nil, ErrStorage
		}
		op.PermitSession = b.SessionID
		op.StartNonce = body.Nonce
		if _, err = tx.ExecContext(ctx, "UPDATE operations SET state='authorized',permit_id=?,permit_session=?,start_nonce=?,auth_rev=?,revision=revision+1,updated_at=? WHERE operation_id=?", op.PermitID, op.PermitSession, op.StartNonce, op.AuthRev, time.Now().UnixMilli(), op.OperationID); err != nil {
			return nil, ErrStorage
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	response, err := protocol.NewMessage("authorize_start", b.SessionID, env.MessageID, map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest, "start_nonce": op.StartNonce, "permit_id": op.PermitID, "auth_rev": op.AuthRev})
	if err != nil {
		return nil, ErrStorage
	}
	return response, nil
}

func (s *Store) RequestCancelFixture(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireActive(ctx, tx); err != nil {
		return err
	}
	op, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return err
	}
	if err = s.requireOperationEvidence(ctx, tx, op); err != nil {
		return err
	}
	if !op.CancelRequested && !terminalState(op.State) {
		if _, err = tx.ExecContext(ctx, "UPDATE operations SET cancel_requested=1,revision=revision+1,updated_at=? WHERE operation_id=?", time.Now().UnixMilli(), id); err != nil {
			return ErrStorage
		}
		if _, err = tx.ExecContext(ctx, "UPDATE outbox SET delivery_state='suppressed' WHERE operation_id=? AND kind='prepare' AND delivery_state='pending'", id); err != nil {
			return ErrStorage
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO outbox(operation_id,kind,generation,delivery_state) VALUES(?,'cancel',1,'pending')", id); err != nil {
			return ErrStorage
		}
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}
