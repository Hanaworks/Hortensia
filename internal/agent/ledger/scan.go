package ledger

import (
	"context"
	"encoding/json"

	"docker-manager/internal/protocol"
)

// Reconcile consumes a structurally valid request from the trusted current
// session. Failures return no successful wire response; the future transport
// adapter owns their protocol error mapping and delivery.
func (s *Store) Reconcile(ctx context.Context, raw []byte) ([]byte, error) {
	if err := s.requireRecovery(); err != nil {
		return nil, err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "reconcile" || env.SessionID == nil {
		return nil, ErrInput
	}
	var req struct {
		Scope  string      `json:"scope"`
		Scan   *string     `json:"scan_id"`
		Cursor *string     `json:"cursor"`
		Limit  json.Number `json:"limit"`
	}
	if err = json.Unmarshal(env.Body, &req); err != nil {
		return nil, ErrInput
	}
	n, err := req.Limit.Float64()
	if err != nil {
		return nil, ErrInput
	}
	limit := int(n)
	if req.Scan == nil && req.Cursor != nil {
		return nil, ErrScanCursor
	}
	cfg := s.recovery
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	if err = s.requireRecoverySession(ctx, s.db, *env.SessionID); err != nil {
		return nil, err
	}
	control, err := readScanControl(ctx, s.db)
	if err != nil {
		return nil, err
	}
	snapshot := cfg.snapshot
	if s.scanExpired(snapshot) {
		snapshot = nil
	}
	live := snapshot != nil && snapshot.id == control.active
	if req.Scan == nil {
		if live && !control.dirty {
			if req.Scope == snapshot.scope {
				if limit != snapshot.limit {
					return nil, ErrScanCursor
				}
			} else if snapshot.scope != "pending" || req.Scope != "all" {
				return nil, ErrScanCursor
			} else {
				live = false
			}
		} else {
			live = false
		}
		if !live {
			next, err := s.freezeAndSnapshot(ctx, *env.SessionID, req.Scope, limit)
			if err != nil {
				return nil, err
			}
			if snapshot != nil {
				snapshot.clear()
			}
			cfg.snapshot = next
			snapshot = next
		}
	} else {
		if !live || *req.Scan != snapshot.id {
			return nil, ErrScanExpired
		}
		if control.dirty {
			return nil, ErrScanChanged
		}
		if req.Scope != snapshot.scope || limit != snapshot.limit {
			return nil, ErrScanCursor
		}
	}
	cursor := ""
	if req.Cursor != nil {
		cursor = *req.Cursor
	}
	return s.scanPage(ctx, env, snapshot, cursor)
}

func (s *Store) scanPage(ctx context.Context, env protocol.Envelope, snapshot *scanSnapshot, cursor string) ([]byte, error) {
	if s.scanExpired(snapshot) {
		return nil, ErrScanExpired
	}
	start, issued := snapshot.issued[cursor]
	if !issued {
		return nil, ErrScanCursor
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	// Waiting for the sole SQLite connection is part of elapsed scan time.
	if s.scanExpired(snapshot) {
		return nil, ErrScanExpired
	}
	if err = s.requireRecoverySession(ctx, tx, *env.SessionID); err != nil {
		return nil, err
	}
	control, err := readScanControl(ctx, tx)
	if err != nil {
		return nil, err
	}
	if control.dirty {
		return nil, ErrScanChanged
	}
	if err = s.verifyScanMembers(ctx, tx, control, snapshot); err != nil {
		return nil, err
	}
	message, err := protocol.NewID()
	if err != nil {
		return nil, ErrStorage
	}
	page, exists := snapshot.pages[cursor]
	if !exists {
		page = scanPage{start: start, end: start}
		next, err := protocol.NewID()
		if err != nil {
			return nil, ErrStorage
		}
		partial, _ := scanWire(*env.SessionID, env.MessageID, snapshot.id, snapshot.scope, s.identity.LedgerID, len(snapshot.records), nil, next, message)
		last, _ := scanWire(*env.SessionID, env.MessageID, snapshot.id, snapshot.scope, s.identity.LedgerID, len(snapshot.records), nil, "", message)
		bytes := 0
		for end := start; end < len(snapshot.records) && end-start < snapshot.limit; end++ {
			addition := len(snapshot.records[end])
			if end > start {
				addition++
			}
			base := len(partial)
			if end+1 == len(snapshot.records) {
				base = len(last)
			}
			if base+bytes+addition > scanWireLimit {
				break
			}
			bytes += addition
			page.end = end + 1
		}
		if start < len(snapshot.records) && page.end == start {
			return nil, ErrScanBusy
		}
		if page.end < len(snapshot.records) {
			page.next = next
		}
	}
	response, err := scanWire(*env.SessionID, env.MessageID, snapshot.id, snapshot.scope, s.identity.LedgerID, len(snapshot.records), snapshot.records[page.start:page.end], page.next, message)
	if err != nil || len(response) > scanWireLimit {
		clear(response)
		return nil, ErrScanBusy
	}
	if _, err = s.validator.Validate(response); err != nil {
		clear(response)
		return nil, ErrEvidence
	}
	if s.scanExpired(snapshot) {
		clear(response)
		return nil, ErrScanExpired
	}
	if err = commitScanRead(tx); err != nil {
		clear(response)
		return nil, err
	}
	if s.scanExpired(snapshot) {
		clear(response)
		return nil, ErrScanExpired
	}
	// Do not issue a cursor or mark the tail if the read transaction failed.
	snapshot.pages[cursor] = page
	if page.next != "" {
		snapshot.issued[page.next] = page.end
	} else {
		snapshot.tail = true
	}
	return response, nil
}

// CompleteReconcile closes only the Agent scan barrier. It neither releases
// claims nor proves that Manager business reconciliation has succeeded.
func (s *Store) CompleteReconcile(ctx context.Context, raw []byte) ([]byte, error) {
	if err := s.requireRecovery(); err != nil {
		return nil, err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "reconcile_complete" || env.SessionID == nil {
		return nil, ErrInput
	}
	var req struct {
		Scan   string `json:"scan_id"`
		Ledger string `json:"ledger_id"`
	}
	if err = json.Unmarshal(env.Body, &req); err != nil {
		return nil, ErrInput
	}
	if req.Ledger != s.identity.LedgerID {
		return nil, ErrIdentity
	}
	cfg := s.recovery
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	s.scanExpired(cfg.snapshot)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoverySession(ctx, tx, *env.SessionID); err != nil {
		return nil, err
	}
	control, err := readScanControl(ctx, tx)
	if err != nil {
		return nil, err
	}
	if req.Scan == control.completed {
		if err = commitScanRead(tx); err != nil {
			return nil, err
		}
		return completedWire(*env.SessionID, env.MessageID, req.Scan, req.Ledger)
	}
	snapshot := cfg.snapshot
	if s.scanExpired(snapshot) || snapshot.id != req.Scan || control.active != req.Scan {
		return nil, ErrScanExpired
	}
	if control.dirty {
		return nil, ErrScanChanged
	}
	if !snapshot.tail {
		return nil, ErrScanIncomplete
	}
	if err = s.verifyScanMembers(ctx, tx, control, snapshot); err != nil {
		return nil, err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM recovery_changes c LEFT JOIN execution_events e ON e.operation_id=c.operation_id AND e.event_seq=c.event_seq WHERE e.operation_id IS NULL OR e.ack_session IS NULL OR e.ack_session<>?`, *env.SessionID).Scan(&pending); err != nil {
		return nil, ErrStorage
	}
	if pending > 0 {
		return nil, ErrScanChanged
	}
	if s.scanExpired(snapshot) {
		return nil, ErrScanExpired
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM recovery_members WHERE scan_id<>?`, snapshot.id); err != nil {
		return nil, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM recovery_changes`); err != nil {
		return nil, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_control SET completed_scan_id=?,completed_scope=?,completed_total=?,active_scan_id='',active_scope='',active_limit=0,active_total=0,dirty=0,frozen=0 WHERE id=1`, snapshot.id, snapshot.scope, len(snapshot.records)); err != nil {
		return nil, ErrStorage
	}
	if s.scanExpired(snapshot) {
		return nil, ErrScanExpired
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	s.setRecoveryLocalFrozen(false)
	snapshot.clear()
	cfg.snapshot = nil
	return completedWire(*env.SessionID, env.MessageID, req.Scan, req.Ledger)
}

// Caller holds recovery.mu. Expiry is semantic immediately and body cleanup is
// lazy on access; persistent members and the freeze are deliberately untouched.
func (s *Store) scanExpired(snapshot *scanSnapshot) bool {
	if snapshot == nil {
		return true
	}
	if s.recovery.now().Before(snapshot.deadline) {
		return false
	}
	if s.recovery.snapshot == snapshot {
		s.recovery.snapshot = nil
	}
	snapshot.clear()
	return true
}

func completedWire(session, reply, scan, ledger string) ([]byte, error) {
	raw, err := protocol.NewMessage("reconcile_completed", session, reply, map[string]string{"scan_id": scan, "ledger_id": ledger})
	if err != nil {
		return nil, ErrStorage
	}
	return raw, nil
}
