package operations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"docker-manager/internal/protocol"
)

type recoveryPage struct {
	Scope    string            `json:"scope"`
	ScanID   string            `json:"scan_id"`
	LedgerID string            `json:"ledger_id"`
	Total    json.Number       `json:"total"`
	Records  []json.RawMessage `json:"records"`
	Next     *string           `json:"next_cursor"`
	Complete bool              `json:"complete"`
	Error    *struct {
		Code string `json:"code"`
	} `json:"error"`
	total int
}
type recoveryRequest struct {
	ID, Cursor string
	Page       int
	Raw        []byte
	Accepted   bool
}

func parseRecoveryPage(raw []byte) (recoveryPage, error) {
	var p recoveryPage
	if json.Unmarshal(raw, &p) != nil {
		return p, ErrInput
	}
	n, err := recoveryNumber(p.Total)
	if err != nil {
		return p, ErrRecovery
	}
	p.total = int(n)
	return p, nil
}
func recoveryPageBinding(r recoveryScan, page int, cursor string) blobBinding {
	context, _ := json.Marshal([]any{r.LedgerID, r.SessionID, r.ManagerBoot, r.AgentBootID, r.Scope, r.ScanID, r.Limit, cursor, page})
	return blobBinding{OperationID: r.RoundID, NodeID: r.NodeID, Digest: string(context), Kind: "recovery_page", Seq: uint64(page)}
}
func (s *Store) readRecoveryRequest(ctx context.Context, q querier, r recoveryScan, id string) (recoveryRequest, error) {
	var req recoveryRequest
	var round, kind string
	err := q.QueryRowContext(ctx, `SELECT message_id,round_id,kind,page_no,cursor,raw,accepted FROM recovery_requests WHERE message_id=?`, id).Scan(&req.ID, &round, &kind, &req.Page, &req.Cursor, &req.Raw, &req.Accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return req, ErrRecovery
	}
	if err != nil {
		return req, ErrStorage
	}
	if round != r.RoundID || kind != "reconcile" {
		return req, ErrRecovery
	}
	if err = s.validateRecoveryRequest(r, req); err != nil {
		return req, err
	}
	return req, nil
}
func (s *Store) validateRecoveryRequest(r recoveryScan, req recoveryRequest) error {
	env, err := s.validator.Validate(req.Raw)
	if err != nil || env.Type != "reconcile" || env.MessageID != req.ID || env.SessionID == nil || *env.SessionID != r.SessionID {
		return ErrEvidence
	}
	var b struct {
		Scope  string      `json:"scope"`
		Scan   *string     `json:"scan_id"`
		Cursor *string     `json:"cursor"`
		Limit  json.Number `json:"limit"`
	}
	if json.Unmarshal(env.Body, &b) != nil {
		return ErrEvidence
	}
	n, err := recoveryNumber(b.Limit)
	if err != nil || n != uint64(r.Limit) || b.Scope != r.Scope || req.Page < 0 || req.Page >= 4096 {
		return ErrEvidence
	}
	if req.Page == 0 {
		if req.Cursor != "" || b.Scan != nil || b.Cursor != nil {
			return ErrEvidence
		}
	} else if b.Scan == nil || *b.Scan != r.ScanID || b.Cursor == nil || *b.Cursor != req.Cursor || req.Cursor == "" {
		return ErrEvidence
	}
	return nil
}

// checkRecoveryPage verifies progress using already authenticated prior pages.
func (s *Store) checkRecoveryPage(ctx context.Context, q querier, r recoveryScan, req recoveryRequest, p recoveryPage, bodyLen int) error {
	if p.total > 4096 || p.Error != nil || req.Page != r.Pages || req.Accepted || r.Status != "collecting" || p.Scope != r.Scope || p.LedgerID != r.LedgerID || !validID(p.ScanID) || (r.ScanID != "" && p.ScanID != r.ScanID) || (r.Total != -1 && p.total != r.Total) || len(p.Records) > r.Limit {
		return ErrRecovery
	}
	if r.Pages >= 4096 || r.Bytes+bodyLen > 32<<20 || r.Seen+len(p.Records) > p.total {
		return ErrRecovery
	}
	if req.Cursor != r.NextCursor {
		return ErrRecovery
	}
	if len(p.Records) == 0 && !(r.Pages == 0 && p.total == 0 && p.Complete) {
		return ErrRecovery
	}
	if p.Complete != (r.Seen+len(p.Records) == p.total) || (p.Complete && p.Next != nil) || (!p.Complete && (p.Next == nil || *p.Next == "")) {
		return ErrRecovery
	}
	last := r.LastOperation
	for _, raw := range p.Records {
		record, err := parseRecoveryRecord(raw)
		if err != nil {
			return err
		}
		if record.OperationID <= last {
			return ErrRecovery
		}
		last = record.OperationID
	}
	if p.Next != nil && q != nil {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM recovery_requests WHERE round_id=? AND kind='reconcile' AND cursor=?`, r.RoundID, *p.Next).Scan(&n); err != nil {
			return ErrStorage
		}
		if n != 0 {
			return ErrRecovery
		}
	}
	return nil
}
func (s *Store) pagePrecheck(ctx context.Context, tx *sql.Tx, b Binding, env protocol.Envelope, p recoveryPage) (recoveryScan, recoveryRequest, bool, error) {
	r, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil {
		return r, recoveryRequest{}, false, err
	}
	req, err := s.readRecoveryRequest(ctx, tx, r, *env.ReplyTo)
	if err != nil {
		return r, req, false, err
	}
	pages, err := s.readRecoveryPages(ctx, tx, r)
	defer func() {
		for _, raw := range pages {
			clear(raw)
		}
	}()
	if err != nil {
		return r, req, false, err
	}
	if req.Accepted {
		if req.Page >= len(pages) {
			return r, req, false, ErrEvidence
		}
		if !bytes.Equal(pages[req.Page], env.Body) {
			return r, req, false, ErrConflict
		}
		return r, req, true, nil
	}
	if err = s.checkRecoveryPage(ctx, tx, r, req, p, len(env.Body)); err != nil {
		return r, req, false, err
	}
	return r, req, false, nil
}
func (s *Store) ReceiveRecoveryPage(ctx context.Context, b Binding, raw []byte) (RecoveryState, error) {
	if err := s.requireRecovery(); err != nil {
		return RecoveryState{}, err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "reconcile_reply" || env.SessionID == nil || *env.SessionID != b.SessionID || env.ReplyTo == nil {
		return RecoveryState{}, ErrInput
	}
	p, err := parseRecoveryPage(env.Body)
	if err != nil {
		return RecoveryState{}, err
	}
	if p.Error != nil {
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return RecoveryState{}, ErrStorage
		}
		current, e := s.loadRecoveryScan(ctx, tx, b)
		if e == nil {
			var req recoveryRequest
			req, e = s.readRecoveryRequest(ctx, tx, current, *env.ReplyTo)
			if e == nil && (p.Scope != current.Scope || p.LedgerID != current.LedgerID || (req.Page == 0 && p.ScanID != "") || (req.Page > 0 && p.ScanID != "" && p.ScanID != current.ScanID)) {
				e = ErrRecovery
			}
		}
		tx.Rollback()
		if e != nil {
			return RecoveryState{}, e
		}
		return s.RecordRecoveryFailureFixture(ctx, b, *env.ReplyTo, p.Error.Code)
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryState{}, ErrStorage
	}
	r, req, duplicate, err := s.pagePrecheck(ctx, tx, b, env, p)
	tx.Rollback()
	if errors.Is(err, ErrConflict) {
		if e := s.putRecoveryCase(ctx, b, r.RoundID, r.ScanID, "page_conflict", env.Body); e != nil {
			return RecoveryState{}, e
		}
		return RecoveryState{}, ErrConflict
	}
	if err != nil {
		return RecoveryState{}, err
	}
	if duplicate {
		return r.RecoveryState, nil
	}
	sealScan := r
	sealScan.ScanID = p.ScanID
	sealed, err := s.seal(ctx, recoveryPageBinding(sealScan, req.Page, req.Cursor), env.Body)
	if err != nil {
		return RecoveryState{}, err
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryState{}, ErrStorage
	}
	defer tx.Rollback()
	r, req, duplicate, err = s.pagePrecheck(ctx, tx, b, env, p)
	if err != nil {
		return RecoveryState{}, err
	}
	if duplicate {
		return r.RecoveryState, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_pages(round_id,page_no,request_id,body_cipher,wrapped_dek,key_id,fingerprint) VALUES(?,?,?,?,?,?,?)`, r.RoundID, req.Page, req.ID, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint); err != nil {
		return RecoveryState{}, ErrStorage
	}
	for i, rawRecord := range p.Records {
		record, _ := parseRecoveryRecord(rawRecord)
		if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_members(round_id,ordinal,operation_id,phase,page_no,record_no) VALUES(?,?,?,?,?,?)`, r.RoundID, r.Seen+i, record.OperationID, record.Phase, req.Page, i); err != nil {
			return RecoveryState{}, ErrStorage
		}
		r.LastOperation = record.OperationID
	}
	r.Pages++
	r.Seen += len(p.Records)
	r.Total = p.total
	r.Bytes += len(env.Body)
	r.ScanID = p.ScanID
	r.NextCursor = ""
	if p.Next != nil {
		r.NextCursor = *p.Next
	}
	if p.Complete {
		r.Status = "enumerated"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_requests SET accepted=1 WHERE message_id=?`, req.ID); err != nil {
		return RecoveryState{}, ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_scans SET scan_id=?,status=?,total=?,seen=?,pages=?,body_bytes=?,next_cursor=?,last_operation=? WHERE id=?`, r.ScanID, r.Status, r.Total, r.Seen, r.Pages, r.Bytes, r.NextCursor, r.LastOperation, r.RoundID); err != nil {
		return RecoveryState{}, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return RecoveryState{}, ErrStorage
	}
	return r.RecoveryState, nil
}

// Transport fixtures report only errors tied to a currently outstanding request.
func (s *Store) RecordRecoveryFailureFixture(ctx context.Context, b Binding, requestID, code string) (RecoveryState, error) {
	if err := s.requireRecovery(); err != nil {
		return RecoveryState{}, err
	}
	if !validID(requestID) || (code != "scan_expired" && code != "scan_changed" && code != "cursor_invalid" && code != "busy") {
		return RecoveryState{}, ErrInput
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryState{}, ErrStorage
	}
	defer tx.Rollback()
	r, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil {
		return RecoveryState{}, err
	}
	var round, kind string
	var page int
	var accepted bool
	if err = tx.QueryRowContext(ctx, `SELECT round_id,kind,page_no,accepted FROM recovery_requests WHERE message_id=?`, requestID).Scan(&round, &kind, &page, &accepted); err != nil {
		return RecoveryState{}, ErrRecovery
	}
	if round != r.RoundID || accepted || !((kind == "reconcile" && r.Status == "collecting" && page == r.Pages) || (kind == "reconcile_complete" && r.Status == "completing" && page == -1)) {
		return RecoveryState{}, ErrRecovery
	}
	if code != "busy" {
		if _, err = tx.ExecContext(ctx, `UPDATE recovery_scans SET status='expired' WHERE id=?`, r.RoundID); err != nil {
			return RecoveryState{}, ErrStorage
		}
		r.Status = "expired"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE nodes SET frozen=1 WHERE id=?`, b.NodeID); err != nil {
		return RecoveryState{}, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return RecoveryState{}, ErrStorage
	}
	return r.RecoveryState, nil
}
