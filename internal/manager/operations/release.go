package operations

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"docker-manager/internal/protocol"
)

func CreateRelease(ctx context.Context, path, managerID string, keys Keys) (*Store, error) {
	return createVersion(ctx, path, managerID, keys, 3)
}
func OpenRelease(ctx context.Context, path string, expected Identity, keys Keys) (*Store, error) {
	return openVersion(ctx, path, expected, keys, 3)
}

type finalizationBody struct {
	OperationID string `json:"operation_id"`
	Digest      string `json:"input_digest"`
	ResultSeq   uint64 `json:"result_event_seq"`
	State       string `json:"terminal_state"`
}

// Numbers use the shared JSON safe-integer representation, including 1e0.
func parseFinalization(raw []byte) (finalizationBody, error) {
	var p struct {
		OperationID string      `json:"operation_id"`
		Digest      string      `json:"input_digest"`
		Seq         json.Number `json:"result_event_seq"`
		State       string      `json:"terminal_state"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return finalizationBody{}, ErrEvidence
	}
	seq, err := recoveryNumber(p.Seq)
	if err != nil || seq < 1 {
		return finalizationBody{}, ErrEvidence
	}
	return finalizationBody{p.OperationID, p.Digest, seq, p.State}, nil
}
func (s *Store) expectedFinalization(ctx context.Context, q querier, op Operation) (finalizationBody, error) {
	if s.version == 5 {
		var pending int
		if e := q.QueryRowContext(ctx, `SELECT count(*) FROM runtime_pending_events WHERE operation_id=?`, op.OperationID).Scan(&pending); e != nil {
			return finalizationBody{}, ErrStorage
		}
		if pending > 0 {
			return finalizationBody{}, ErrRecoveryIncomplete
		}
	}
	if op.Blocked {
		return finalizationBody{}, ErrFrozen
	}
	if op.LatestResultSeq == 0 || !terminalState(op.State) || (op.ReleaseStatus != "pending" && op.ReleaseStatus != "released") {
		return finalizationBody{}, ErrRecoveryIncomplete
	}
	e, err := s.readEvent(ctx, q, op, op.LatestResultSeq)
	if err != nil {
		return finalizationBody{}, err
	}
	defer clear(e.Body)
	if e.Kind != "result" {
		return finalizationBody{}, ErrEvidence
	}
	p, err := parseEvent(e.Body)
	if err != nil {
		return finalizationBody{}, err
	}
	state, release := resultProjection(p)
	if release != "pending" || state != op.State {
		return finalizationBody{}, ErrEvidence
	}
	return finalizationBody{op.OperationID, op.Digest, p.seq, state}, nil
}

type finalizeRequest struct {
	ID, OperationID, NodeID, LedgerID, SessionID, ManagerBoot, AgentBoot, RoundID string
	Raw                                                                           []byte
	Accepted                                                                      bool
	ConfirmationMAC                                                               []byte
}

// Each accepted request is a separate authenticated fact, including when an
// operation was already released on an earlier connection or recovery round.
// The purpose-separated MAC uses no KEK wrapping quota and binds the exact
// request bytes as well as the independently verified result projection.
func (s *Store) finalizeConfirmationMAC(r finalizeRequest, body finalizationBody) ([]byte, error) {
	keys, err := s.keyCopies()
	if err != nil {
		return nil, err
	}
	defer clear(keys.DigestKey)
	defer clear(keys.EncryptionKey)
	context, err := json.Marshal([]any{s.identity.ManagerID, r.ID, r.OperationID, r.NodeID, r.LedgerID, r.SessionID, r.ManagerBoot, r.AgentBoot, r.RoundID, string(r.Raw), body.OperationID, body.Digest, body.ResultSeq, body.State})
	if err != nil {
		return nil, ErrEvidence
	}
	mac := hmac.New(sha256.New, keys.DigestKey)
	mac.Write([]byte("dm-manager-finalize-confirmation-v1\x00"))
	mac.Write(context)
	return mac.Sum(nil), nil
}

func (s *Store) readFinalizeRequest(ctx context.Context, q querier, id string, op Operation) (finalizeRequest, error) {
	var r finalizeRequest
	err := q.QueryRowContext(ctx, `SELECT message_id,operation_id,node_id,ledger_id,session_id,manager_boot,agent_boot,round_id,raw,accepted,confirmation_mac FROM finalize_requests WHERE message_id=?`, id).Scan(&r.ID, &r.OperationID, &r.NodeID, &r.LedgerID, &r.SessionID, &r.ManagerBoot, &r.AgentBoot, &r.RoundID, &r.Raw, &r.Accepted, &r.ConfirmationMAC)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, ErrStorage
	}
	e, err := s.validator.Validate(r.Raw)
	if err != nil || e.Type != "finalize" || e.MessageID != r.ID || e.SessionID == nil || *e.SessionID != r.SessionID || r.OperationID != op.OperationID || r.NodeID != op.NodeID || r.LedgerID != op.LedgerID || !validID(r.ManagerBoot) || !validID(r.AgentBoot) || (r.RoundID != "" && !validID(r.RoundID)) {
		return r, ErrEvidence
	}
	body, err := parseFinalization(e.Body)
	if err != nil {
		return r, err
	}
	want, err := s.expectedFinalization(ctx, q, op)
	if errors.Is(err, ErrStorage) {
		return r, err
	}
	if err != nil || body != want {
		return r, ErrEvidence
	}
	if r.RoundID != "" {
		scan, err := s.readRecoveryScanByID(ctx, q, r.RoundID)
		if errors.Is(err, ErrStorage) {
			return r, err
		}
		if err != nil || scan.NodeID != r.NodeID || scan.LedgerID != r.LedgerID || scan.SessionID != r.SessionID || scan.ManagerBoot != r.ManagerBoot || scan.AgentBootID != r.AgentBoot {
			return r, ErrEvidence
		}
	}
	if r.Accepted {
		if op.ReleaseStatus != "released" || len(r.ConfirmationMAC) != sha256.Size {
			return r, ErrEvidence
		}
		mac, err := s.finalizeConfirmationMAC(r, want)
		if err != nil {
			return r, err
		}
		if !hmac.Equal(mac, r.ConfirmationMAC) {
			return r, ErrEvidence
		}
	} else if len(r.ConfirmationMAC) != 0 {
		return r, ErrEvidence
	}
	return r, nil
}

// Completion invalidates no audit evidence, but requests outside an active
// recovery use an empty round. A new round always requires fresh confirmation.
func (s *Store) finalizeContext(ctx context.Context, q querier, b Binding) (string, string, error) {
	if err := s.requireBinding(ctx, q, b, true); err != nil {
		return "", "", err
	}
	blocked, err := s.recoveryQuarantined(ctx, q, b.NodeID)
	if err != nil {
		return "", "", err
	}
	if blocked {
		return "", "", ErrFrozen
	}
	var cases int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM recovery_cases WHERE node_id=?`, b.NodeID).Scan(&cases); err != nil {
		return "", "", ErrStorage
	}
	if cases != 0 {
		return "", "", ErrFrozen
	}
	var agent, round string
	if err = q.QueryRowContext(ctx, `SELECT agent_boot,active_round FROM recovery_nodes WHERE node_id=?`, b.NodeID).Scan(&agent, &round); err != nil {
		return "", "", ErrEvidence
	}
	if !validID(agent) {
		return "", "", ErrEvidence
	}
	if round != "" {
		scan, err := s.loadRecoveryScan(ctx, q, b)
		if err != nil {
			return "", "", err
		}
		if scan.Status == "expired" || scan.Status == "blocked" {
			return "", "", ErrFrozen
		}
		if scan.Status == "completed" {
			round = ""
		}
	}
	return agent, round, nil
}
func (s *Store) prepareFinalize(ctx context.Context, b Binding, id string) ([]byte, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	agent, round, err := s.finalizeContext(ctx, tx, b)
	if err != nil {
		return nil, err
	}
	op, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if op.NodeID != b.NodeID || op.LedgerID != b.LedgerID {
		return nil, ErrIdentity
	}
	if err = s.requireOperationEvidence(ctx, tx, op); err != nil {
		return nil, err
	}
	body, err := s.expectedFinalization(ctx, tx, op)
	if err != nil {
		return nil, err
	}
	e, err := s.readEvent(ctx, tx, op, body.ResultSeq)
	if err != nil {
		return nil, err
	}
	p, _ := parseEvent(e.Body)
	err = s.validateEventAgainstRecovery(ctx, tx, op, "result", p, e.Body)
	clear(e.Body)
	if err != nil {
		return nil, err
	}
	var requestID string
	err = tx.QueryRowContext(ctx, `SELECT message_id FROM finalize_requests WHERE operation_id=? AND node_id=? AND ledger_id=? AND session_id=? AND manager_boot=? AND agent_boot=? AND round_id=?`, id, b.NodeID, b.LedgerID, b.SessionID, s.boot, agent, round).Scan(&requestID)
	var raw []byte
	if err == nil {
		r, err := s.readFinalizeRequest(ctx, tx, requestID, op)
		if err != nil {
			return nil, err
		}
		raw = r.Raw
	} else if errors.Is(err, sql.ErrNoRows) {
		raw, err = protocol.NewMessage("finalize", b.SessionID, "", body)
		if err != nil {
			return nil, ErrStorage
		}
		env, err := s.validator.Validate(raw)
		if err != nil {
			return nil, ErrEvidence
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO finalize_requests(message_id,operation_id,node_id,ledger_id,session_id,manager_boot,agent_boot,round_id,raw,accepted) VALUES(?,?,?,?,?,?,?,?,?,0)`, env.MessageID, id, b.NodeID, b.LedgerID, b.SessionID, s.boot, agent, round, raw); err != nil {
			return nil, ErrStorage
		}
	} else {
		return nil, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	return raw, nil
}
func (s *Store) PrepareFinalize(ctx context.Context, b Binding, id string) ([]byte, error) {
	if s.version != 3 && s.version != 4 && s.version != 5 {
		return nil, ErrSchema
	}
	if !validID(id) {
		return nil, ErrInput
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	raw, err := s.prepareFinalize(ctx, b, id)
	if errors.Is(err, ErrConflict) {
		evidence, _ := json.Marshal(map[string]string{"operation_id": id, "reason": "finalize contradicts durable evidence"})
		if caseErr := s.putRecoveryCase(ctx, b, "", id, "finalize_conflict", evidence); caseErr != nil {
			return nil, caseErr
		}
	}
	return raw, err
}
func finalizationBinding(op Operation, request finalizeRequest) blobBinding {
	context, _ := json.Marshal([]string{op.LedgerID, op.Digest, request.ID, request.ManagerBoot, request.AgentBoot, request.SessionID, request.RoundID, string(request.Raw)})
	return blobBinding{OperationID: op.OperationID, NodeID: op.NodeID, Digest: string(context), Kind: "finalized", Seq: op.LatestResultSeq}
}

// This helper deliberately does not call requireOperationEvidence: claims and
// event projection validation both use it within the caller's transaction.
func (s *Store) readFinalized(ctx context.Context, q querier, op Operation) (json.RawMessage, error) {
	var blob sealedBlob
	var request, state string
	var seq uint64
	err := q.QueryRowContext(ctx, `SELECT request_id,result_event_seq,terminal_state,body_cipher,wrapped_dek,key_id,fingerprint FROM finalization WHERE operation_id=?`, op.OperationID).Scan(&request, &seq, &state, &blob.Cipher, &blob.WrappedDEK, &blob.KeyID, &blob.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, ErrStorage
	}
	want, err := s.expectedFinalization(ctx, q, op)
	if errors.Is(err, ErrStorage) {
		return nil, err
	}
	if err != nil || seq != want.ResultSeq || state != want.State || op.ReleaseStatus != "released" {
		return nil, ErrEvidence
	}
	r, err := s.readFinalizeRequest(ctx, q, request, op)
	if errors.Is(err, ErrStorage) {
		return nil, err
	}
	if err != nil || !r.Accepted {
		return nil, ErrEvidence
	}
	raw, err := s.unseal(finalizationBinding(op, r), blob)
	if err != nil {
		return nil, err
	}
	e, err := s.validator.Validate(bodyEnvelope("finalize", raw))
	if err != nil {
		clear(raw)
		return nil, ErrEvidence
	}
	actual, err := parseFinalization(e.Body)
	if err != nil || actual != want {
		clear(raw)
		return nil, ErrEvidence
	}
	var claims, absence int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM resource_claims WHERE operation_id=?`, op.OperationID).Scan(&claims); err != nil {
		clear(raw)
		return nil, ErrStorage
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM absence_releases WHERE operation_id=?`, op.OperationID).Scan(&absence); err != nil {
		clear(raw)
		return nil, ErrStorage
	}
	if claims != 0 || absence != 0 {
		clear(raw)
		return nil, ErrEvidence
	}
	var intentState, terminal string
	var intentSeq uint64
	err = q.QueryRowContext(ctx, `SELECT delivery_state,result_event_seq,terminal_state FROM outbox WHERE operation_id=? AND kind='finalize' AND generation=1`, op.OperationID).Scan(&intentState, &intentSeq, &terminal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		clear(raw)
		return nil, ErrStorage
	}
	if err != nil || intentState != "confirmed" || intentSeq != seq || terminal != state {
		clear(raw)
		return nil, ErrEvidence
	}
	return raw, nil
}
func (s *Store) verifyRelease(ctx context.Context, q querier, op Operation) error {
	if (s.version == 3 || (s.version == 4 || s.version == 5)) && op.LatestResultSeq > 0 {
		raw, err := s.readFinalized(ctx, q, op)
		clear(raw)
		if errors.Is(err, ErrNotFound) {
			return ErrEvidence
		}
		return err
	}
	if s.version == 3 || (s.version == 4 || s.version == 5) {
		var receipts int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM finalization WHERE operation_id=?`, op.OperationID).Scan(&receipts); err != nil {
			return ErrStorage
		}
		if receipts != 0 {
			return ErrEvidence
		}
	}
	return s.verifyAbsenceRelease(ctx, q, op)
}
func (s *Store) ReadFinalized(ctx context.Context, id string) (json.RawMessage, error) {
	if s.version != 3 && s.version != 4 && s.version != 5 {
		return nil, ErrSchema
	}
	if !validID(id) {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	op, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if op.ReleaseStatus != "released" || op.LatestResultSeq == 0 {
		return nil, ErrNotFound
	}
	if err = s.requireOperationEvidence(ctx, tx, op); err != nil {
		return nil, err
	}
	return s.readFinalized(ctx, tx, op)
}
func (s *Store) receiveFinalized(ctx context.Context, b Binding, env protocol.Envelope, body finalizationBody, sealed *sealedBlob) (bool, Operation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, Operation{}, ErrStorage
	}
	defer tx.Rollback()
	agent, round, err := s.finalizeContext(ctx, tx, b)
	if err != nil {
		return false, Operation{}, err
	}
	// Reject unknown/obsolete correlation before inspecting the body, so a
	// delayed response cannot quarantine the new connection's business facts.
	var id string
	err = tx.QueryRowContext(ctx, `SELECT operation_id FROM finalize_requests WHERE message_id=? AND node_id=? AND ledger_id=? AND session_id=? AND manager_boot=? AND agent_boot=? AND round_id=?`, *env.ReplyTo, b.NodeID, b.LedgerID, b.SessionID, s.boot, agent, round).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, Operation{}, ErrInput
	}
	if err != nil {
		return false, Operation{}, ErrStorage
	}
	op, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return false, op, err
	}
	if err = s.requireOperationEvidence(ctx, tx, op); err != nil {
		return false, op, err
	}
	r, err := s.readFinalizeRequest(ctx, tx, *env.ReplyTo, op)
	if err != nil {
		return false, op, err
	}
	want, err := s.expectedFinalization(ctx, tx, op)
	if err != nil {
		return false, op, err
	}
	if body != want {
		return false, op, ErrConflict
	}
	e, err := s.readEvent(ctx, tx, op, want.ResultSeq)
	if err != nil {
		return false, op, err
	}
	p, _ := parseEvent(e.Body)
	err = s.validateEventAgainstRecovery(ctx, tx, op, "result", p, e.Body)
	clear(e.Body)
	if err != nil {
		return false, op, err
	}
	if op.ReleaseStatus == "released" {
		// Authenticated original body is retained even if JSON formatting differs.
		saved, err := s.readFinalized(ctx, tx, op)
		if err != nil {
			return false, op, err
		}
		clear(saved)
	} else {
		if r.Accepted {
			return false, op, ErrEvidence
		}
		if sealed == nil {
			return true, op, nil
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO finalization(operation_id,request_id,result_event_seq,terminal_state,body_cipher,wrapped_dek,key_id,fingerprint) VALUES(?,?,?,?,?,?,?,?)`, op.OperationID, r.ID, body.ResultSeq, body.State, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint); err != nil {
			return false, op, ErrStorage
		}
		if _, err = tx.ExecContext(ctx, `UPDATE operations SET release_status='released',revision=revision+1,updated_at=? WHERE operation_id=?`, time.Now().UnixMilli(), op.OperationID); err != nil {
			return false, op, ErrStorage
		}
		if _, err = tx.ExecContext(ctx, `UPDATE outbox SET delivery_state='confirmed' WHERE operation_id=? AND kind='finalize' AND generation=1`, op.OperationID); err != nil {
			return false, op, ErrStorage
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM resource_claims WHERE operation_id=?`, op.OperationID); err != nil {
			return false, op, ErrStorage
		}
	}
	if !r.Accepted {
		mac, err := s.finalizeConfirmationMAC(r, want)
		if err != nil {
			return false, op, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE finalize_requests SET accepted=1,confirmation_mac=? WHERE message_id=?`, mac, r.ID); err != nil {
			return false, op, ErrStorage
		}
	}
	if err = tx.Commit(); err != nil {
		return false, op, ErrStorage
	}
	return false, op, nil
}
func (s *Store) ReceiveFinalized(ctx context.Context, b Binding, raw []byte) error {
	if s.version != 3 && s.version != 4 && s.version != 5 {
		return ErrSchema
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "finalized" || env.SessionID == nil || *env.SessionID != b.SessionID || env.ReplyTo == nil {
		return ErrInput
	}
	body, err := parseFinalization(env.Body)
	if err != nil {
		return ErrInput
	}
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	needs, op, err := s.receiveFinalized(ctx, b, env, body, nil)
	if err == nil && needs {
		var blob sealedBlob
		r, readErr := s.readFinalizeRequest(ctx, s.db, *env.ReplyTo, op)
		if readErr != nil {
			return readErr
		}
		blob, err = s.seal(ctx, finalizationBinding(op, r), env.Body)
		if err == nil {
			_, op, err = s.receiveFinalized(ctx, b, env, body, &blob)
		}
	}
	if errors.Is(err, ErrConflict) {
		if caseErr := s.putRecoveryCase(ctx, b, "", op.OperationID, "finalized_conflict", env.Body); caseErr != nil {
			return caseErr
		}
	}
	return err
}

// Authentic same-event replays do not become new facts after release.
func sameReleasedEvent(existing Event, env protocol.Envelope) bool {
	return existing.Kind == env.Type && bytes.Equal(existing.Body, env.Body)
}

// A seq2 result may legitimately precede started(seq1), including across the
// release transaction. Save that missing compatible fact without re-projecting
// the terminal outcome, confirming intents again, or rebuilding claims.
func (s *Store) receiveReleasedEvent(ctx context.Context, tx *sql.Tx, op Operation, env protocol.Envelope, p parsedEvent, sealed *sealedBlob) (bool, error) {
	existing, err := s.readEvent(ctx, tx, op, p.seq)
	if err == nil {
		defer clear(existing.Body)
		if !sameReleasedEvent(existing, env) {
			return false, ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return false, ErrStorage
		}
		return false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if env.Type != "started" || p.seq != 1 || (op.LatestResultSeq != 2 && !(s.version == 5 && op.LatestResultSeq > 2)) || op.PermitID == "" || op.PermitID != p.PermitID {
		return false, ErrConflict
	}
	if err = s.validateEventAgainstRecovery(ctx, tx, op, env.Type, p, env.Body); err != nil {
		return false, err
	}
	if sealed == nil {
		return true, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_events(operation_id,source,event_seq,kind,body_cipher,wrapped_dek,key_id,fingerprint,received_at) VALUES(?,'agent',?,?,?,?,?,?,?)`, op.OperationID, p.seq, env.Type, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint, time.Now().UnixMilli()); err != nil {
		return false, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return false, ErrStorage
	}
	return false, nil
}
