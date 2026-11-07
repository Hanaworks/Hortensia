package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"docker-manager/internal/protocol"
)

func (s *Store) recoveryCompleteRequest(ctx context.Context, q querier, scan recoveryScan) ([]byte, string, bool, error) {
	var raw []byte
	var id string
	var accepted bool
	err := q.QueryRowContext(ctx, `SELECT message_id,raw,accepted FROM recovery_requests WHERE round_id=? AND kind='reconcile_complete' AND page_no=-1 AND cursor=''`, scan.RoundID).Scan(&id, &raw, &accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, ErrNotFound
	}
	if err != nil {
		return nil, "", false, ErrStorage
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "reconcile_complete" || env.MessageID != id || env.SessionID == nil || *env.SessionID != scan.SessionID {
		return nil, "", false, ErrEvidence
	}
	var body struct {
		Scan   string `json:"scan_id"`
		Ledger string `json:"ledger_id"`
	}
	if json.Unmarshal(env.Body, &body) != nil || body.Scan != scan.ScanID || body.Ledger != scan.LedgerID {
		return nil, "", false, ErrEvidence
	}
	return raw, id, accepted, nil
}

func (s *Store) PrepareRecoveryComplete(ctx context.Context, b Binding) ([]byte, error) {
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
	scan, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil {
		return nil, err
	}
	if scan.Status != "ready" && scan.Status != "completing" {
		return nil, ErrRecoveryIncomplete
	}
	if err = s.recoveryBusinessReady(ctx, tx, scan); err != nil {
		return nil, err
	}
	raw, _, accepted, err := s.recoveryCompleteRequest(ctx, tx, scan)
	if err == nil {
		if accepted || scan.Status != "completing" {
			return nil, ErrEvidence
		}
	} else if errors.Is(err, ErrNotFound) && scan.Status == "ready" {
		raw, err = protocol.NewMessage("reconcile_complete", b.SessionID, "", map[string]string{"scan_id": scan.ScanID, "ledger_id": b.LedgerID})
		if err != nil {
			return nil, ErrStorage
		}
		env, err := s.validator.Validate(raw)
		if err != nil {
			return nil, ErrEvidence
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_requests(message_id,round_id,kind,page_no,cursor,raw,accepted) VALUES(?,?,'reconcile_complete',-1,'',?,0)`, env.MessageID, scan.RoundID, raw); err != nil {
			return nil, ErrStorage
		}
		if _, err = tx.ExecContext(ctx, `UPDATE recovery_scans SET status='completing' WHERE id=?`, scan.RoundID); err != nil {
			return nil, ErrStorage
		}
	} else {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	return raw, nil
}

func (s *Store) ReceiveRecoveryCompleted(ctx context.Context, b Binding, raw []byte) (RecoveryState, error) {
	if err := s.requireRecovery(); err != nil {
		return RecoveryState{}, err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "reconcile_completed" || env.SessionID == nil || *env.SessionID != b.SessionID || env.ReplyTo == nil {
		return RecoveryState{}, ErrInput
	}
	var body struct {
		Scan   string `json:"scan_id"`
		Ledger string `json:"ledger_id"`
	}
	if json.Unmarshal(env.Body, &body) != nil {
		return RecoveryState{}, ErrInput
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryState{}, ErrStorage
	}
	defer tx.Rollback()
	scan, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil {
		return RecoveryState{}, err
	}
	if body.Scan != scan.ScanID || body.Ledger != b.LedgerID {
		return scan.RecoveryState, ErrIdentity
	}
	_, id, accepted, err := s.recoveryCompleteRequest(ctx, tx, scan)
	if err != nil {
		return scan.RecoveryState, err
	}
	if id != *env.ReplyTo {
		return scan.RecoveryState, ErrInput
	}
	if scan.Status == "completed" && accepted {
		// Historical replay is read-only even if a later conflict has frozen
		// this node. Never rewrite frozen=0 from an old accepted receipt.
		if err = tx.Commit(); err != nil {
			return scan.RecoveryState, ErrStorage
		}
		return scan.RecoveryState, nil
	}
	if scan.Status != "completing" || accepted {
		return scan.RecoveryState, ErrRecoveryIncomplete
	}
	if err = s.recoveryBusinessReady(ctx, tx, scan); err != nil {
		return scan.RecoveryState, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_requests SET accepted=1 WHERE message_id=?`, id); err != nil {
		return scan.RecoveryState, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_scans SET status='completed' WHERE id=?`, scan.RoundID); err != nil {
		return scan.RecoveryState, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE nodes SET frozen=0 WHERE id=?`, b.NodeID); err != nil {
		return scan.RecoveryState, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return scan.RecoveryState, ErrStorage
	}
	scan.Status = "completed"
	return scan.RecoveryState, nil
}
