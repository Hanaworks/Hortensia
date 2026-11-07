package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"time"

	"docker-manager/internal/protocol"
)

const scanLifetime = 15 * time.Minute
const scanWireLimit = 8 << 20

type scanMember struct {
	OperationID        string
	LastSeq, ResultSeq uint64
	Phase              string
	ReportAcked        bool
	Fingerprint        []byte
}
type scanPage struct {
	start, end int
	next       string
}
type scanSnapshot struct {
	id, scope string
	limit     int
	deadline  time.Time
	records   []json.RawMessage
	members   []scanMember
	issued    map[string]int
	pages     map[string]scanPage
	tail      bool
}

func (s *scanSnapshot) clear() {
	if s == nil {
		return
	}
	for _, r := range s.records {
		clear(r)
	}
	s.records = nil
	s.members = nil
	s.pages = nil
	s.issued = nil
	s.tail = false
}

type scanControl struct {
	boot, session, active, scope, completed, completedScope string
	frozen, dirty                                           bool
	limit                                                   int
	total, completedTotal                                   uint64
}

func readScanControl(ctx context.Context, q querier) (scanControl, error) {
	var c scanControl
	err := q.QueryRowContext(ctx, `SELECT active_boot,current_session,frozen,active_scan_id,active_scope,active_limit,active_total,dirty,completed_scan_id,completed_scope,completed_total FROM recovery_control WHERE id=1`).Scan(&c.boot, &c.session, &c.frozen, &c.active, &c.scope, &c.limit, &c.total, &c.dirty, &c.completed, &c.completedScope, &c.completedTotal)
	if err != nil {
		return c, ErrStorage
	}
	return c, nil
}

func (s *Store) scanFingerprint(raw []byte) ([]byte, error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, ErrStorage
	}
	key := append([]byte(nil), s.key...)
	s.mu.Unlock()
	defer clear(key)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("dm-scan-record-v1\x00"))
	mac.Write(raw)
	return mac.Sum(nil), nil
}

// scanWire writes only the envelope and fixed page metadata through Marshal.
// Every RecoveryRecord, including its encrypted journal's original result body,
// is appended byte for byte without RawMessage re-encoding.
func scanWire(session, reply, id, scope, ledger string, total int, records []json.RawMessage, next string, message string) ([]byte, error) {
	var cursor any
	if next != "" {
		cursor = next
	}
	head, err := json.Marshal(struct {
		Protocol  int    `json:"protocol"`
		Type      string `json:"type"`
		MessageID string `json:"message_id"`
		Session   string `json:"session_id"`
		Reply     string `json:"reply_to"`
	}{1, "reconcile_reply", message, session, reply})
	if err != nil {
		return nil, ErrStorage
	}
	meta, err := json.Marshal(struct {
		Scope    string `json:"scope"`
		Scan     string `json:"scan_id"`
		Ledger   string `json:"ledger_id"`
		Total    int    `json:"total"`
		Next     any    `json:"next_cursor"`
		Complete bool   `json:"complete"`
		Error    any    `json:"error"`
	}{scope, id, ledger, total, cursor, next == "", nil})
	if err != nil {
		return nil, ErrStorage
	}
	raw := append(head[:len(head)-1], []byte(`,"body":`)...)
	raw = append(raw, meta[:len(meta)-1]...)
	raw = append(raw, []byte(`,"records":[`)...)
	for i, r := range records {
		if i > 0 {
			raw = append(raw, ',')
		}
		raw = append(raw, r...)
	}
	raw = append(raw, ']', '}', '}')
	return raw, nil
}

// freezeAndSnapshot is called with recovery.mu, but never with Store.mu or a
// database transaction while invalidating Gate callbacks.
func (s *Store) freezeAndSnapshot(ctx context.Context, session, scope string, limit int) (*scanSnapshot, error) {
	s.freezeRecoveryGates()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	if err = s.requireRecoverySession(ctx, tx, session); err != nil {
		tx.Rollback()
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_control SET frozen=1 WHERE id=1`); err != nil {
		tx.Rollback()
		return nil, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	return s.buildScanSnapshot(ctx, session, scope, limit)
}

func (s *Store) buildScanSnapshot(ctx context.Context, session, scope string, limit int) (*scanSnapshot, error) {
	id, err := protocol.NewID()
	if err != nil {
		return nil, ErrStorage
	}
	snapshot := &scanSnapshot{id: id, scope: scope, limit: limit, deadline: s.recovery.now().Add(scanLifetime), issued: map[string]int{"": 0}, pages: make(map[string]scanPage)}
	success := false
	defer func() {
		if !success {
			snapshot.clear()
		}
	}()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoverySession(ctx, tx, session); err != nil {
		return nil, err
	}
	control, err := readScanControl(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !control.frozen {
		return nil, ErrFrozen
	}
	// All executions in v3's current first-result slice are unfinalized. Keep
	// scope distinct even though pending and all currently enumerate the same IDs.
	rows, err := tx.QueryContext(ctx, `SELECT operation_id FROM executions ORDER BY operation_id LIMIT ?`, s.recovery.maxRecords+1)
	if err != nil {
		return nil, ErrStorage
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, ErrStorage
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return nil, ErrStorage
	}
	if len(ids) > s.recovery.maxRecords {
		return nil, ErrScanBusy
	}
	used := 0
	for _, operation := range ids {
		raw, err := s.readRecoveryRecord(ctx, tx, operation)
		if err != nil {
			return nil, err
		}
		used += len(raw)
		if used > s.recovery.maxBytes {
			clear(raw)
			return nil, ErrScanBusy
		}
		// Validate each complete projection before publishing any members. The
		// original body bytes are untouched by this validation-only envelope.
		probe, err := scanWire(session, id, id, scope, s.identity.LedgerID, 1, []json.RawMessage{raw}, "", id)
		if err != nil || len(probe) > scanWireLimit {
			clear(raw)
			return nil, ErrScanBusy
		}
		_, err = s.validator.Validate(probe)
		clear(probe)
		if err != nil {
			clear(raw)
			return nil, ErrEvidence
		}
		var view struct {
			Operation string  `json:"operation_id"`
			Last      uint64  `json:"last_event_seq"`
			Result    *uint64 `json:"result_event_seq"`
			Phase     string  `json:"phase"`
			Ack       bool    `json:"report_acked"`
		}
		if err = json.Unmarshal(raw, &view); err != nil || view.Operation != operation {
			clear(raw)
			return nil, ErrEvidence
		}
		fingerprint, err := s.scanFingerprint(raw)
		if err != nil {
			clear(raw)
			return nil, err
		}
		member := scanMember{OperationID: operation, LastSeq: view.Last, Phase: view.Phase, ReportAcked: view.Ack, Fingerprint: fingerprint}
		if view.Result != nil {
			member.ResultSeq = *view.Result
		}
		snapshot.records = append(snapshot.records, raw)
		snapshot.members = append(snapshot.members, member)
	}
	// A replacement is published only after its full coherent snapshot exists;
	// failed upgrades retain the prior active scan and latest completed members.
	if _, err = tx.ExecContext(ctx, `DELETE FROM recovery_members WHERE scan_id<>?`, control.completed); err != nil {
		return nil, ErrStorage
	}
	for i, m := range snapshot.members {
		if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_members(scan_id,ordinal,operation_id,last_event_seq,result_event_seq,phase,report_acked,record_fingerprint) VALUES(?,?,?,?,?,?,?,?)`, id, i, m.OperationID, m.LastSeq, m.ResultSeq, m.Phase, m.ReportAcked, m.Fingerprint); err != nil {
			return nil, ErrStorage
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_control SET active_scan_id=?,active_scope=?,active_limit=?,active_total=?,dirty=0 WHERE id=1`, id, scope, limit, len(snapshot.records)); err != nil {
		return nil, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	success = true
	return snapshot, nil
}

func (s *Store) verifyScanMembers(ctx context.Context, q querier, c scanControl, snapshot *scanSnapshot) error {
	if c.active != snapshot.id || c.scope != snapshot.scope || c.limit != snapshot.limit || c.total != uint64(len(snapshot.records)) || !c.frozen {
		return ErrEvidence
	}
	rows, err := q.QueryContext(ctx, `SELECT ordinal,operation_id,last_event_seq,result_event_seq,phase,report_acked,record_fingerprint FROM recovery_members WHERE scan_id=? ORDER BY ordinal`, snapshot.id)
	if err != nil {
		return ErrStorage
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var ordinal int
		var m scanMember
		if err = rows.Scan(&ordinal, &m.OperationID, &m.LastSeq, &m.ResultSeq, &m.Phase, &m.ReportAcked, &m.Fingerprint); err != nil {
			return ErrStorage
		}
		if i >= len(snapshot.members) {
			return ErrEvidence
		}
		want := snapshot.members[i]
		if ordinal != i || m.OperationID != want.OperationID || m.LastSeq != want.LastSeq || m.ResultSeq != want.ResultSeq || m.Phase != want.Phase || m.ReportAcked != want.ReportAcked || !hmac.Equal(m.Fingerprint, want.Fingerprint) {
			return ErrEvidence
		}
		i++
	}
	if rows.Err() != nil {
		return ErrStorage
	}
	if i != len(snapshot.members) {
		return ErrEvidence
	}
	return nil
}

func commitScanRead(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}
