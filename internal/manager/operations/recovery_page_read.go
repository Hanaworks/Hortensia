package operations

import (
	"context"
	"encoding/json"
)

type recoveryStoredPage struct {
	Page    int
	Request string
	Blob    sealedBlob
}

// All pages, requests and members are checked within the caller's snapshot.
// Counters are only comparisons against reconstructed authenticated progress.
func (s *Store) readRecoveryPages(ctx context.Context, q querier, r recoveryScan) (out []json.RawMessage, err error) {
	defer func() {
		if err != nil {
			for _, raw := range out {
				clear(raw)
			}
			out = nil
		}
	}()
	rows, err := q.QueryContext(ctx, `SELECT page_no,request_id,body_cipher,wrapped_dek,key_id,fingerprint FROM recovery_pages WHERE round_id=? ORDER BY page_no`, r.RoundID)
	if err != nil {
		return nil, ErrStorage
	}
	var stored []recoveryStoredPage
	cipherBytes := 0
	for rows.Next() {
		var p recoveryStoredPage
		if err = rows.Scan(&p.Page, &p.Request, &p.Blob.Cipher, &p.Blob.WrappedDEK, &p.Blob.KeyID, &p.Blob.Fingerprint); err != nil {
			rows.Close()
			return nil, ErrStorage
		}
		cipherBytes += len(p.Blob.Cipher)
		stored = append(stored, p)
		if len(stored) > 4096 || cipherBytes > (32<<20)+28*len(stored) {
			rows.Close()
			return nil, ErrEvidence
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ErrStorage
	}
	if len(stored) != r.Pages {
		return nil, ErrEvidence
	}
	progress := r
	progress.Pages = 0
	progress.Seen = 0
	progress.Total = -1
	progress.Bytes = 0
	progress.LastOperation = ""
	progress.NextCursor = ""
	progress.ScanID = ""
	progress.Status = "collecting"
	type member struct {
		ID, Phase    string
		Page, Record int
	}
	var members []member
	cursors := map[string]bool{"": true}
	complete := false
	for i, p := range stored {
		if p.Page != i || complete {
			return out, ErrEvidence
		}
		req, e := s.readRecoveryRequest(ctx, q, r, p.Request)
		if e != nil {
			return out, e
		}
		if !req.Accepted || req.Page != i {
			return out, ErrEvidence
		}
		raw, e := s.unseal(recoveryPageBinding(r, i, req.Cursor), p.Blob)
		if e != nil {
			return out, e
		}
		out = append(out, raw)
		// Keep original body bytes; reconstruct only a schema-validation envelope.
		head, _ := json.Marshal(map[string]any{"protocol": 1, "type": "reconcile_reply", "message_id": validationID, "session_id": r.SessionID, "reply_to": req.ID})
		envelope := append(head[:len(head)-1], []byte(`,"body":`)...)
		envelope = append(envelope, raw...)
		envelope = append(envelope, '}')
		_, e = s.validator.Validate(envelope)
		clear(envelope)
		if e != nil {
			return out, ErrEvidence
		}
		body, e := parseRecoveryPage(raw)
		if e != nil {
			return out, ErrEvidence
		}
		req.Accepted = false
		if e = s.checkRecoveryPage(ctx, nil, progress, req, body, len(raw)); e != nil {
			return out, ErrEvidence
		}
		if body.Next != nil {
			if cursors[*body.Next] {
				return out, ErrEvidence
			}
			cursors[*body.Next] = true
		}
		for j, recordRaw := range body.Records {
			record, e := parseRecoveryRecord(recordRaw)
			if e != nil {
				return out, ErrEvidence
			}
			members = append(members, member{record.OperationID, record.Phase, i, j})
			progress.LastOperation = record.OperationID
		}
		progress.Pages++
		progress.Seen += len(body.Records)
		progress.Total = body.total
		progress.Bytes += len(raw)
		progress.ScanID = body.ScanID
		progress.NextCursor = ""
		if body.Next != nil {
			progress.NextCursor = *body.Next
		}
		complete = body.Complete
	}
	if progress.Seen != r.Seen || progress.Total != r.Total || progress.Bytes != r.Bytes || progress.LastOperation != r.LastOperation || progress.NextCursor != r.NextCursor || progress.ScanID != r.ScanID {
		return out, ErrEvidence
	}
	switch r.Status {
	case "enumerated", "need_all", "waiting", "ready", "completing", "completed":
		if !complete {
			return out, ErrEvidence
		}
	case "collecting":
		if complete {
			return out, ErrEvidence
		}
	}
	rows, err = q.QueryContext(ctx, `SELECT ordinal,operation_id,phase,page_no,record_no FROM recovery_members WHERE round_id=? ORDER BY ordinal`, r.RoundID)
	if err != nil {
		return out, ErrStorage
	}
	n := 0
	for rows.Next() {
		var ordinal, page, record int
		var id, phase string
		if err = rows.Scan(&ordinal, &id, &phase, &page, &record); err != nil {
			rows.Close()
			return out, ErrStorage
		}
		if n >= len(members) || ordinal != n || id != members[n].ID || phase != members[n].Phase || page != members[n].Page || record != members[n].Record {
			rows.Close()
			return out, ErrEvidence
		}
		n++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, ErrStorage
	}
	if n != len(members) {
		return out, ErrEvidence
	}
	// Detect orphan/misassociated/extra accepted requests, including an altered raw
	// request that was never used to fetch a page. The pending request is replayed.
	rows, err = q.QueryContext(ctx, `SELECT message_id,page_no,cursor,raw,accepted FROM recovery_requests WHERE round_id=? AND kind='reconcile' ORDER BY page_no`, r.RoundID)
	if err != nil {
		return out, ErrStorage
	}
	n = 0
	for rows.Next() {
		var req recoveryRequest
		if err = rows.Scan(&req.ID, &req.Page, &req.Cursor, &req.Raw, &req.Accepted); err != nil {
			rows.Close()
			return out, ErrStorage
		}
		if req.Page != n || n > len(stored) || s.validateRecoveryRequest(r, req) != nil {
			rows.Close()
			return out, ErrEvidence
		}
		if n < len(stored) {
			if !req.Accepted || req.ID != stored[n].Request {
				rows.Close()
				return out, ErrEvidence
			}
		} else if req.Accepted || complete || req.Cursor != r.NextCursor {
			rows.Close()
			return out, ErrEvidence
		}
		n++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, ErrStorage
	}
	if n < len(stored) {
		return out, ErrEvidence
	}
	return out, nil
}
func (s *Store) ReadRecoveryPage(ctx context.Context, roundID string, pageNo int) (json.RawMessage, error) {
	if err := s.requireRecovery(); err != nil {
		return nil, err
	}
	if !validID(roundID) || pageNo < 0 || pageNo >= 4096 {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	r, err := s.readRecoveryScanByID(ctx, tx, roundID)
	if err != nil {
		return nil, err
	}
	pages, err := s.readRecoveryPages(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	if pageNo >= len(pages) {
		for _, raw := range pages {
			clear(raw)
		}
		return nil, ErrNotFound
	}
	raw := pages[pageNo]
	for i, p := range pages {
		if i != pageNo {
			clear(p)
		}
	}
	return raw, nil
}
