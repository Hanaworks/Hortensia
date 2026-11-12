package operations

import (
	"context"
	"database/sql"
	"errors"

	"docker-manager/internal/protocol"
)

var (
	ErrRecovery           = errors.New("manager recovery invalid")
	ErrRecoveryIncomplete = errors.New("manager recovery incomplete")
	ErrRecoveryBusy       = errors.New("manager recovery already active")
)

type RecoveryState struct {
	RoundID, ScanID, Scope, Status, AgentBootID, NextCursor string
	Limit, Pages, Seen, Total                               int
}
type recoveryScan struct {
	RecoveryState
	NodeID, LedgerID, SessionID, ManagerBoot, LastOperation string
	Bytes                                                   int
}

func CreateRecovery(ctx context.Context, path, managerID string, keys Keys) (*Store, error) {
	return createVersion(ctx, path, managerID, keys, 2)
}
func OpenRecovery(ctx context.Context, path string, expected Identity, keys Keys) (*Store, error) {
	return openVersion(ctx, path, expected, keys, 2)
}
func (s *Store) requireRecovery() error {
	if s.version != 2 && s.version != 3 && s.version != 4 && s.version != 5 {
		return ErrSchema
	}
	return nil
}

// The trusted hello supplies Agent boot independently of the session binding.
// Rebinding abandons completion eligibility without deleting audit evidence.
func (s *Store) BindRecoverySessionFixture(ctx context.Context, b Binding, agentBootID string) error {
	if s.version == 4 || s.version == 5 {
		return ErrSchema
	}
	if err := s.requireRecovery(); err != nil {
		return err
	}
	if !validID(b.NodeID) || !validID(b.LedgerID) || !validID(b.SessionID) || !validID(agentBootID) {
		return ErrInput
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireActive(ctx, tx); err != nil {
		return err
	}
	var ledger, session, boot, agentBoot, round string
	err = tx.QueryRowContext(ctx, `SELECT n.ledger_id,n.session_id,n.boot_id,r.agent_boot,r.active_round FROM nodes n JOIN recovery_nodes r ON r.node_id=n.id WHERE n.id=?`, b.NodeID).Scan(&ledger, &session, &boot, &agentBoot, &round)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, `INSERT INTO nodes(id,ledger_id,session_id,boot_id,frozen) VALUES(?,?,?,?,1)`, b.NodeID, b.LedgerID, b.SessionID, s.boot); err != nil {
			return ErrStorage
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO recovery_nodes(node_id,agent_boot,active_round) VALUES(?,?,'')`, b.NodeID, agentBootID)
	case err != nil:
		return ErrStorage
	case ledger != b.LedgerID:
		return ErrIdentity
	case session != b.SessionID || boot != s.boot || agentBoot != agentBootID:
		if round != "" {
			if _, err = tx.ExecContext(ctx, `UPDATE recovery_scans SET status='abandoned' WHERE id=?`, round); err != nil {
				return ErrStorage
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE nodes SET session_id=?,boot_id=?,frozen=1 WHERE id=?`, b.SessionID, s.boot, b.NodeID); err != nil {
			return ErrStorage
		}
		_, err = tx.ExecContext(ctx, `UPDATE recovery_nodes SET agent_boot=?,active_round='' WHERE node_id=?`, agentBootID, b.NodeID)
	}
	if err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) readRecoveryScanByID(ctx context.Context, q querier, id string) (recoveryScan, error) {
	var r recoveryScan
	err := q.QueryRowContext(ctx, `SELECT id,node_id,ledger_id,session_id,manager_boot,agent_boot,scope,page_limit,scan_id,status,total,seen,pages,body_bytes,next_cursor,last_operation FROM recovery_scans WHERE id=?`, id).Scan(&r.RoundID, &r.NodeID, &r.LedgerID, &r.SessionID, &r.ManagerBoot, &r.AgentBootID, &r.Scope, &r.Limit, &r.ScanID, &r.Status, &r.Total, &r.Seen, &r.Pages, &r.Bytes, &r.NextCursor, &r.LastOperation)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, ErrStorage
	}
	if !validID(r.RoundID) || !validID(r.NodeID) || !validID(r.LedgerID) || !validID(r.SessionID) || !validID(r.ManagerBoot) || !validID(r.AgentBootID) || (r.ScanID != "" && !validID(r.ScanID)) || (r.Scope != "pending" && r.Scope != "all") || r.Limit < 1 || r.Limit > 200 || r.Pages < 0 || r.Pages > 4096 || r.Seen < 0 || r.Seen > 4096 || r.Total < -1 || r.Total > 4096 || r.Bytes < 0 || r.Bytes > 32<<20 {
		return recoveryScan{}, ErrEvidence
	}
	return r, nil
}
func (s *Store) loadRecoveryScan(ctx context.Context, q querier, b Binding) (recoveryScan, error) {
	if err := s.requireRecovery(); err != nil {
		return recoveryScan{}, err
	}
	if err := s.requireBinding(ctx, q, b, true); err != nil {
		return recoveryScan{}, err
	}
	var round, agentBoot string
	if err := q.QueryRowContext(ctx, `SELECT active_round,agent_boot FROM recovery_nodes WHERE node_id=?`, b.NodeID).Scan(&round, &agentBoot); err != nil {
		return recoveryScan{}, ErrEvidence
	}
	if round == "" {
		return recoveryScan{}, ErrNotFound
	}
	r, err := s.readRecoveryScanByID(ctx, q, round)
	if err != nil {
		return r, err
	}
	if r.NodeID != b.NodeID || r.LedgerID != b.LedgerID || r.SessionID != b.SessionID || r.ManagerBoot != s.boot || r.AgentBootID != agentBoot || r.Status == "abandoned" {
		return recoveryScan{}, ErrFrozen
	}
	return r, nil
}
func (s *Store) ReadRecoveryState(ctx context.Context, nodeID string) (RecoveryState, error) {
	if err := s.requireRecovery(); err != nil {
		return RecoveryState{}, err
	}
	if !validID(nodeID) {
		return RecoveryState{}, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryState{}, ErrStorage
	}
	defer tx.Rollback()
	var round string
	if err = tx.QueryRowContext(ctx, `SELECT active_round FROM recovery_nodes WHERE node_id=?`, nodeID).Scan(&round); errors.Is(err, sql.ErrNoRows) {
		return RecoveryState{}, ErrNotFound
	} else if err != nil {
		return RecoveryState{}, ErrStorage
	}
	if round == "" {
		return RecoveryState{}, ErrNotFound
	}
	r, err := s.readRecoveryScanByID(ctx, tx, round)
	return r.RecoveryState, err
}
func (s *Store) StartRecovery(ctx context.Context, b Binding, scope string, limit int) ([]byte, error) {
	if err := s.requireRecovery(); err != nil {
		return nil, err
	}
	if (scope != "all" && scope != "pending") || limit < 1 || limit > 200 {
		return nil, ErrInput
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireBinding(ctx, tx, b, true); err != nil {
		return nil, err
	}
	old, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err == nil && old.Status != "expired" && old.Status != "completed" && !(old.Scope == "pending" && scope == "all") {
		if old.Scope != scope || old.Limit != limit {
			return nil, ErrRecoveryBusy
		}
		raw, e := s.nextRecoveryRequest(ctx, tx, old)
		if e != nil {
			return nil, e
		}
		if e = tx.Commit(); e != nil {
			return nil, ErrStorage
		}
		return raw, nil
	}
	if old.RoundID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE recovery_scans SET status='abandoned' WHERE id=?`, old.RoundID); err != nil {
			return nil, ErrStorage
		}
	}
	var boot string
	if err = tx.QueryRowContext(ctx, `SELECT agent_boot FROM recovery_nodes WHERE node_id=?`, b.NodeID).Scan(&boot); err != nil {
		return nil, ErrEvidence
	}
	id, err := protocol.NewID()
	if err != nil {
		return nil, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_scans(id,node_id,ledger_id,session_id,manager_boot,agent_boot,scope,page_limit,status) VALUES(?,?,?,?,?,?,?,?,'collecting')`, id, b.NodeID, b.LedgerID, b.SessionID, s.boot, boot, scope, limit); err != nil {
		return nil, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_expected(round_id,operation_id,last_event_seq) SELECT ?,operation_id,max_event_seq FROM operations WHERE node_id=?`, id, b.NodeID); err != nil {
		return nil, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_nodes SET active_round=? WHERE node_id=?`, id, b.NodeID); err != nil {
		return nil, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE nodes SET frozen=1 WHERE id=?`, b.NodeID); err != nil {
		return nil, ErrStorage
	}
	r, err := s.readRecoveryScanByID(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	raw, err := s.nextRecoveryRequest(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	return raw, nil
}
func (s *Store) NextRecoveryRequest(ctx context.Context, b Binding) ([]byte, error) {
	if err := s.requireRecovery(); err != nil {
		return nil, err
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	r, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil {
		return nil, err
	}
	raw, err := s.nextRecoveryRequest(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	return raw, nil
}
func (s *Store) nextRecoveryRequest(ctx context.Context, tx *sql.Tx, r recoveryScan) ([]byte, error) {
	if r.Status != "collecting" {
		return nil, ErrRecoveryIncomplete
	}
	pages, err := s.readRecoveryPages(ctx, tx, r)
	for _, raw := range pages {
		clear(raw)
	}
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT raw FROM recovery_requests WHERE round_id=? AND kind='reconcile' AND page_no=? AND accepted=0`, r.RoundID, r.Pages).Scan(&raw)
	if err == nil {
		return raw, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, ErrStorage
	}
	var scan, cursor any
	if r.Pages > 0 {
		scan = r.ScanID
		cursor = r.NextCursor
	}
	raw, err = protocol.NewMessage("reconcile", r.SessionID, "", map[string]any{"scope": r.Scope, "limit": r.Limit, "scan_id": scan, "cursor": cursor})
	if err != nil {
		return nil, ErrStorage
	}
	env, err := s.validator.Validate(raw)
	if err != nil {
		return nil, ErrEvidence
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_requests(message_id,round_id,kind,page_no,cursor,raw,accepted) VALUES(?,?,'reconcile',?,?,?,0)`, env.MessageID, r.RoundID, r.Pages, r.NextCursor, raw); err != nil {
		return nil, ErrStorage
	}
	return raw, nil
}
