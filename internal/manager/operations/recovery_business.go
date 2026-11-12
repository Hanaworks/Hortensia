package operations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"docker-manager/internal/protocol"
)

func recoveryRecords(pages []json.RawMessage) ([]recoveryRecord, error) {
	var records []recoveryRecord
	for _, raw := range pages {
		var page struct {
			Records []json.RawMessage `json:"records"`
		}
		if json.Unmarshal(raw, &page) != nil {
			return nil, ErrEvidence
		}
		for _, rawRecord := range page.Records {
			r, err := parseRecoveryRecord(rawRecord)
			if err != nil {
				return nil, err
			}
			records = append(records, r)
		}
	}
	return records, nil
}

func clearRecoveryPages(pages []json.RawMessage) {
	for _, raw := range pages {
		clear(raw)
	}
}

func clearRecoveryRecords(records []recoveryRecord) {
	for _, r := range records {
		clear(r.Raw)
		clear(r.Result)
	}
}

func operationIDs(ctx context.Context, q querier, node string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT operation_id FROM operations WHERE node_id=? ORDER BY operation_id`, node)
	if err != nil {
		return nil, ErrStorage
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			return nil, ErrStorage
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil {
		return nil, ErrStorage
	}
	return ids, nil
}

// Import a body through the existing event transaction logic without inventing
// a network message ID or returning an ack for an embedded scan result.
func (s *Store) importRecoveryResult(ctx context.Context, b Binding, raw []byte) error {
	p, err := parseEvent(raw)
	if err != nil || !eventPosition("result", p) {
		return ErrInput
	}
	env := protocol.Envelope{Type: "result", SessionID: &b.SessionID, Body: raw}
	needsSeal, err := s.receiveEvent(ctx, b, env, p, nil)
	if err != nil {
		return err
	}
	if !needsSeal {
		return nil
	}
	sealed, err := s.seal(ctx, blobBinding{OperationID: p.OperationID, NodeID: b.NodeID, Digest: p.Digest, Kind: "result", Seq: p.seq}, raw)
	if err != nil {
		return err
	}
	_, err = s.receiveEvent(ctx, b, env, p, &sealed)
	return err
}

func (s *Store) ProcessRecovery(ctx context.Context, b Binding) (RecoveryState, error) {
	if err := s.requireRecovery(); err != nil {
		return RecoveryState{}, err
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecoveryState{}, ErrStorage
	}
	scan, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil {
		tx.Rollback()
		return RecoveryState{}, err
	}
	if scan.Status == "collecting" || scan.Status == "expired" || scan.Status == "completing" || scan.Status == "completed" || scan.Total < 0 || scan.Seen != scan.Total || scan.NextCursor != "" || scan.Pages == 0 {
		tx.Rollback()
		return scan.RecoveryState, ErrRecoveryIncomplete
	}
	pages, err := s.readRecoveryPages(ctx, tx, scan)
	if err != nil {
		tx.Rollback()
		return scan.RecoveryState, err
	}
	defer clearRecoveryPages(pages)
	if err = tx.Commit(); err != nil {
		return scan.RecoveryState, ErrStorage
	}
	records, err := recoveryRecords(pages)
	if err != nil {
		return scan.RecoveryState, err
	}
	defer clearRecoveryRecords(records)
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		seen[r.OperationID] = true
		kind := ""
		op, readErr := s.Get(ctx, r.OperationID)
		switch {
		case errors.Is(readErr, ErrNotFound):
			kind = "unknown_operation"
		case errors.Is(readErr, ErrEvidence):
			kind = "damaged_operation"
		case readErr != nil:
			return scan.RecoveryState, readErr
		case op.NodeID != b.NodeID || op.LedgerID != b.LedgerID || op.Digest != r.Digest || op.StackID != r.StackID || op.VersionID != r.VersionID || op.ProjectName != r.ProjectName:
			kind = "record_mismatch"
		case op.ReleaseStatus == "released" && (s.version < 3 || op.LatestResultSeq == 0):
			kind = "released_record"
		case r.Phase == "reconciliation_required":
			kind = "agent_reconciliation_required"
		case r.Phase == "START_COMMITTED" && (op.PermitID == "" || op.LatestResultSeq == 1):
			kind = "started_evidence_conflict"
		case s.unsupportedRecoveryHistory(r):
			kind = "unsupported_history"
		}
		if kind != "" {
			if err = s.putRecoveryCase(ctx, b, scan.RoundID, r.OperationID, kind, r.Raw); err != nil {
				return scan.RecoveryState, err
			}
			continue
		}
		if r.Result != nil {
			err = s.importRecoveryResult(ctx, b, r.Result)
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrFrozen) || errors.Is(err, ErrEvidence) || errors.Is(err, ErrInput) || errors.Is(err, errRecoveryReleased) {
				if err = s.putRecoveryCase(ctx, b, scan.RoundID, r.OperationID, "result_conflict", r.Raw); err != nil {
					return scan.RecoveryState, err
				}
			} else if err != nil {
				return scan.RecoveryState, err
			}
		} else if op.PermitID == "" && op.LatestResultSeq == 0 && !op.Blocked && (r.Phase == "ready" || r.Phase == "preparing") {
			if err = s.RequestCancelFixture(ctx, op.OperationID); err != nil {
				return scan.RecoveryState, err
			}
		}
	}
	ids, err := operationIDs(ctx, s.db, b.NodeID)
	if err != nil {
		return scan.RecoveryState, err
	}
	needAll := false
	for _, id := range ids {
		if seen[id] {
			continue
		}
		op, err := s.Get(ctx, id)
		if errors.Is(err, ErrEvidence) {
			evidence, _ := json.Marshal(map[string]string{"operation_id": id, "scan_id": scan.ScanID, "reason": "local evidence invalid"})
			if err = s.putRecoveryCase(ctx, b, scan.RoundID, id, "damaged_operation", evidence); err != nil {
				return scan.RecoveryState, err
			}
			continue
		}
		if err != nil {
			return scan.RecoveryState, err
		}
		if op.ReleaseStatus == "released" && (s.version < 3 || op.LatestResultSeq == 0 || scan.Scope == "pending") {
			continue
		}
		if scan.Scope == "pending" {
			needAll = true
			continue
		}
		if op.PermitID != "" || op.MaxEventSeq > 0 || op.Blocked {
			evidence, _ := json.Marshal(map[string]string{"operation_id": id, "scan_id": scan.ScanID, "reason": "authorized or observed operation absent from all scan"})
			if err = s.putRecoveryCase(ctx, b, scan.RoundID, id, "missing_history", evidence); err != nil {
				return scan.RecoveryState, err
			}
			continue
		}
		if err = s.releaseAbsent(ctx, b, scan.RoundID, id); err != nil && !errors.Is(err, ErrFrozen) {
			return scan.RecoveryState, err
		}
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return scan.RecoveryState, ErrStorage
	}
	defer tx.Rollback()
	current, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil {
		return scan.RecoveryState, err
	}
	if current.RoundID != scan.RoundID {
		return current.RecoveryState, ErrFrozen
	}
	status := "waiting"
	var cases int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM recovery_cases WHERE node_id=?`, b.NodeID).Scan(&cases); err != nil {
		return scan.RecoveryState, ErrStorage
	}
	quarantined, err := s.recoveryQuarantined(ctx, tx, b.NodeID)
	if err != nil {
		return scan.RecoveryState, err
	}
	if cases > 0 || quarantined {
		status = "blocked"
	} else if needAll {
		status = "need_all"
	} else {
		err = s.recoveryBusinessReady(ctx, tx, current)
		if err == nil {
			status = "ready"
		} else if !errors.Is(err, ErrRecoveryIncomplete) {
			return scan.RecoveryState, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_scans SET status=? WHERE id=?`, status, scan.RoundID); err != nil {
		return scan.RecoveryState, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return scan.RecoveryState, ErrStorage
	}
	scan.Status = status
	return scan.RecoveryState, nil
}

func (s *Store) releaseAbsent(ctx context.Context, b Binding, round, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	scan, err := s.loadRecoveryScan(ctx, tx, b)
	if err != nil {
		return err
	}
	if scan.RoundID != round || scan.Scope != "all" || scan.Total < 0 || scan.Seen != scan.Total || scan.NextCursor != "" || scan.Pages == 0 {
		return ErrRecoveryIncomplete
	}
	pages, err := s.readRecoveryPages(ctx, tx, scan)
	if err != nil {
		return err
	}
	defer clearRecoveryPages(pages)
	records, err := recoveryRecords(pages)
	if err != nil {
		return err
	}
	defer clearRecoveryRecords(records)
	for _, r := range records {
		if r.OperationID == id {
			return ErrConflict
		}
	}
	var cases int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM recovery_cases WHERE node_id=?`, b.NodeID).Scan(&cases); err != nil {
		return ErrStorage
	}
	if cases != 0 {
		return ErrFrozen
	}
	quarantined, err := s.recoveryQuarantined(ctx, tx, b.NodeID)
	if err != nil {
		return err
	}
	if quarantined {
		return ErrFrozen
	}
	op, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return err
	}
	if err = s.requireOperationEvidence(ctx, tx, op); err != nil {
		return err
	}
	if op.NodeID != b.NodeID || op.LedgerID != b.LedgerID || op.PermitID != "" || op.MaxEventSeq != 0 || op.Blocked {
		return ErrConflict
	}
	if op.ReleaseStatus == "released" {
		return nil
	}
	if op.State != "accepted" && op.State != "ready" {
		return ErrConflict
	}
	var prior uint64
	if err = tx.QueryRowContext(ctx, `SELECT last_event_seq FROM recovery_expected WHERE round_id=? AND operation_id=?`, round, id).Scan(&prior); err != nil || prior != 0 {
		return ErrEvidence
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO absence_releases(operation_id,round_id) VALUES(?,?)`, id, round); err != nil {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET state='canceled',release_status='released',cancel_requested=1,revision=revision+1,updated_at=? WHERE operation_id=?`, time.Now().UnixMilli(), id); err != nil {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbox SET delivery_state='suppressed' WHERE operation_id=? AND kind='prepare'`, id); err != nil {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbox(operation_id,kind,generation,delivery_state) VALUES(?,'cancel',1,'suppressed') ON CONFLICT(operation_id,kind,generation) DO UPDATE SET delivery_state='suppressed'`, id); err != nil {
		return ErrStorage
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM resource_claims WHERE operation_id=?`, id); err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

// A released string is never sufficient. Each read authenticates the retained
// complete all-scan evidence and checks the exceptional local cancellation.
func (s *Store) verifyAbsenceRelease(ctx context.Context, q querier, op Operation) error {
	if s.version < 2 || op.ReleaseStatus != "released" || op.State != "canceled" || !op.CancelRequested || op.PermitID != "" || op.MaxEventSeq != 0 || op.LatestResultSeq != 0 || op.Blocked {
		return ErrEvidence
	}
	var round string
	if err := q.QueryRowContext(ctx, `SELECT round_id FROM absence_releases WHERE operation_id=?`, op.OperationID).Scan(&round); err != nil {
		return ErrEvidence
	}
	scan, err := s.readRecoveryScanByID(ctx, q, round)
	if err != nil {
		return err
	}
	if scan.Scope != "all" || scan.NodeID != op.NodeID || scan.LedgerID != op.LedgerID || scan.Total < 0 || scan.Seen != scan.Total || scan.Pages == 0 || scan.NextCursor != "" {
		return ErrEvidence
	}
	var prior uint64
	if err = q.QueryRowContext(ctx, `SELECT last_event_seq FROM recovery_expected WHERE round_id=? AND operation_id=?`, round, op.OperationID).Scan(&prior); err != nil || prior != 0 {
		return ErrEvidence
	}
	pages, err := s.readRecoveryPages(ctx, q, scan)
	if err != nil {
		return err
	}
	defer clearRecoveryPages(pages)
	records, err := recoveryRecords(pages)
	if err != nil {
		return err
	}
	defer clearRecoveryRecords(records)
	for _, r := range records {
		if r.OperationID == op.OperationID {
			return ErrEvidence
		}
	}
	var n int
	for _, query := range []string{`SELECT count(*) FROM operation_events WHERE operation_id=?`, `SELECT count(*) FROM resource_claims WHERE operation_id=?`} {
		if err = q.QueryRowContext(ctx, query, op.OperationID).Scan(&n); err != nil {
			return ErrStorage
		}
		if n != 0 {
			return ErrEvidence
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT kind,delivery_state FROM outbox WHERE operation_id=? ORDER BY kind`, op.OperationID)
	if err != nil {
		return ErrStorage
	}
	defer rows.Close()
	kinds := map[string]bool{}
	for rows.Next() {
		var kind, state string
		if rows.Scan(&kind, &state) != nil {
			return ErrStorage
		}
		if (kind != "prepare" && kind != "cancel") || state != "suppressed" {
			return ErrEvidence
		}
		kinds[kind] = true
	}
	if rows.Err() != nil {
		return ErrStorage
	}
	if len(kinds) != 2 {
		return ErrEvidence
	}
	return nil
}

func (s *Store) recoveryBusinessReady(ctx context.Context, q querier, scan recoveryScan) error {
	quarantined, err := s.recoveryQuarantined(ctx, q, scan.NodeID)
	if err != nil {
		return err
	}
	if quarantined {
		return ErrRecoveryIncomplete
	}
	if scan.Pages == 0 || scan.Total < 0 || scan.Seen != scan.Total || scan.NextCursor != "" {
		return ErrRecoveryIncomplete
	}
	pages, err := s.readRecoveryPages(ctx, q, scan)
	if err != nil {
		return err
	}
	defer clearRecoveryPages(pages)
	// The legacy v2 format only supports absence releases. V3 additionally
	// requires authenticated receipts and confirmation on this exact round.
	if s.version < 3 && scan.Seen != 0 {
		return ErrRecoveryIncomplete
	}
	seen := map[string]bool{}
	if s.version == 3 {
		records, err := recoveryRecords(pages)
		if err != nil {
			return err
		}
		defer clearRecoveryRecords(records)
		for _, r := range records {
			op, err := s.readOperation(ctx, q, r.OperationID)
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					return err
				}
				return ErrRecoveryIncomplete
			}
			if op.NodeID != scan.NodeID || op.LedgerID != scan.LedgerID || op.Digest != r.Digest || op.StackID != r.StackID || op.VersionID != r.VersionID || op.ProjectName != r.ProjectName || op.LatestResultSeq == 0 || r.Phase == "reconciliation_required" || s.unsupportedRecoveryHistory(r) {
				return ErrRecoveryIncomplete
			}
			if r.Phase == "START_COMMITTED" && (op.PermitID == "" || op.LatestResultSeq == 1) {
				return ErrRecoveryIncomplete
			}
			if r.Result != nil {
				e, err := s.readEvent(ctx, q, op, r.ResultSeq)
				if err != nil {
					return err
				}
				same := e.Kind == "result" && bytes.Equal(e.Body, r.Result)
				clear(e.Body)
				if !same {
					return ErrRecoveryIncomplete
				}
			}
			if op.ReleaseStatus != "released" {
				return ErrRecoveryIncomplete
			}
			var id string
			if err = q.QueryRowContext(ctx, `SELECT message_id FROM finalize_requests WHERE operation_id=? AND node_id=? AND ledger_id=? AND session_id=? AND manager_boot=? AND agent_boot=? AND round_id=? AND accepted=1`, op.OperationID, scan.NodeID, scan.LedgerID, scan.SessionID, scan.ManagerBoot, scan.AgentBootID, scan.RoundID).Scan(&id); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return ErrStorage
				}
				return ErrRecoveryIncomplete
			}
			request, err := s.readFinalizeRequest(ctx, q, id, op)
			if err != nil {
				return err
			}
			if !request.Accepted {
				return ErrRecoveryIncomplete
			}
			seen[r.OperationID] = true
		}
	}
	var n int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM recovery_cases WHERE node_id=?`, scan.NodeID).Scan(&n); err != nil {
		return ErrStorage
	}
	if n != 0 {
		return ErrRecoveryIncomplete
	}
	ids, err := operationIDs(ctx, q, scan.NodeID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		op, err := s.readOperation(ctx, q, id)
		if err != nil {
			return err
		}
		if err = s.requireOperationEvidence(ctx, q, op); err != nil {
			return err
		}
		if op.ReleaseStatus != "released" {
			return ErrRecoveryIncomplete
		}
		if s.version == 3 && scan.Scope == "all" && op.LatestResultSeq > 0 && !seen[id] {
			return ErrRecoveryIncomplete
		}
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM resource_claims WHERE node_id=?`, scan.NodeID).Scan(&n); err != nil {
		return ErrStorage
	}
	if n != 0 {
		return ErrRecoveryIncomplete
	}
	return nil
}
