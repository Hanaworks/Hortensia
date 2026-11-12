package ledger

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"

	"docker-manager/internal/protocol"
)

//go:embed migrations/004_release.sql
var schemaV4 string

// CreateRelease creates a new v4 ledger; existing formats are never migrated.
func CreateRelease(ctx context.Context, path, nodeID, keyID string, key []byte, journalKey JournalKey, options RecoveryOptions) (*Store, error) {
	return createStore(ctx, path, nodeID, keyID, key, &journalKey, storeFormat{version: 4, schema: schemaV4, recovery: &options, release: true})
}

// OpenRelease preserves release/quarantine evidence and freezes the new boot.
func OpenRelease(ctx context.Context, path string, expected Identity, key []byte, journalKey JournalKey, options RecoveryOptions) (*Store, error) {
	return openStore(ctx, path, expected, key, &journalKey, storeFormat{version: 4, schema: schemaV4, recovery: &options, release: true})
}

type finalizedBody struct {
	Operation string `json:"operation_id"`
	Digest    string `json:"input_digest"`
	Seq       uint64 `json:"result_event_seq"`
	Terminal  string `json:"terminal_state"`
}

func parseFinalize(body []byte) (finalizedBody, error) {
	var p struct {
		Operation string      `json:"operation_id"`
		Digest    string      `json:"input_digest"`
		Seq       json.Number `json:"result_event_seq"`
		Terminal  string      `json:"terminal_state"`
	}
	if json.Unmarshal(body, &p) != nil {
		return finalizedBody{}, ErrInput
	}
	n, err := p.Seq.Float64()
	if err != nil || n < 1 || n > 9007199254740991 || n != float64(uint64(n)) {
		return finalizedBody{}, ErrInput
	}
	return finalizedBody{p.Operation, p.Digest, uint64(n), p.Terminal}, nil
}

// releaseTerminal consumes authenticated, schema-valid result bytes only.
// A normal_exit fixture does not prove Docker ownership or publish a baseline.
func releaseTerminal(body []byte) (string, error) {
	var p struct {
		Outcome     string       `json:"outcome"`
		Ownership   string       `json:"ownership_status"`
		Exit        *json.Number `json:"exit_code"`
		Observation *struct {
			Status string `json:"status"`
		} `json:"observation"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &p) != nil {
		return "", ErrEvidence
	}
	if p.Outcome == "not_started" {
		if p.Error != nil && p.Error.Code == "canceled_before_start" {
			return "canceled", nil
		}
		return "failed", nil
	}
	if p.Outcome == "normal_exit" && p.Ownership == "consistent" && p.Observation != nil && p.Observation.Status == "complete" && p.Exit != nil {
		n, err := p.Exit.Float64()
		if err == nil {
			if n == 0 {
				return "succeeded", nil
			}
			return "failed", nil
		}
	}
	return "", ErrConflict
}

func (s *Store) requireRelease() error {
	if err := s.requireRecovery(); err != nil {
		return err
	}
	if !s.release {
		return ErrRecovery
	}
	return nil
}

func (s *Store) requireReleaseHealthy(ctx context.Context, q querier) error {
	if !s.release {
		return nil
	}
	s.mu.Lock()
	local := s.quarantined
	s.mu.Unlock()
	if local {
		return ErrFrozen
	}
	var quarantined bool
	if err := q.QueryRowContext(ctx, `SELECT quarantined FROM release_control WHERE id=1`).Scan(&quarantined); err != nil {
		return ErrEvidence
	}
	if quarantined {
		return ErrFrozen
	}
	return nil
}

// Set the local barrier before fallible writes. Invalidate gates only after the
// transaction ends; their callbacks may query this Store's sole SQL connection.
func (s *Store) quarantineRelease(ctx context.Context, tx *sql.Tx) error {
	s.mu.Lock()
	s.quarantined = true
	s.recovery.localFrozen = true
	s.mu.Unlock()
	if _, err := tx.ExecContext(ctx, `UPDATE release_control SET quarantined=1 WHERE id=1`); err != nil {
		return ErrStorage
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recovery_control SET frozen=1,dirty=1 WHERE id=1`); err != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) freezeQuarantinedGates() {
	s.mu.Lock()
	q := s.quarantined
	s.mu.Unlock()
	if q {
		s.freezeRecoveryGates()
	}
}

// Diagnostic reads remain possible after quarantine; they grant no admission
// or successful finalize response. This helper never obtains a new connection.
func (s *Store) readFinalization(ctx context.Context, q querier, r Record, state eventState) (json.RawMessage, finalizedBody, error) {
	if !s.release {
		return nil, finalizedBody{}, ErrNotFound
	}
	var seq uint64
	var terminal string
	var sealed sealedEvent
	err := q.QueryRowContext(ctx, `SELECT result_event_seq,terminal_state,body_cipher,wrapped_dek,key_id,body_fingerprint FROM finalizations WHERE operation_id=?`, r.OperationID).Scan(&seq, &terminal, &sealed.Cipher, &sealed.WrappedDEK, &sealed.KeyID, &sealed.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, finalizedBody{}, ErrNotFound
	}
	if err != nil {
		return nil, finalizedBody{}, ErrStorage
	}
	body, err := s.openEvent(eventBinding{OperationID: r.OperationID, Digest: r.Digest, Kind: "finalized", Seq: seq}, sealed)
	if err != nil {
		return nil, finalizedBody{}, err
	}
	p, err := parseFinalize(body)
	if err != nil || p != (finalizedBody{r.OperationID, r.Digest, seq, terminal}) || !state.LatestResultSeq.Valid || uint64(state.LatestResultSeq.Int64) != seq {
		return nil, finalizedBody{}, ErrEvidence
	}
	probe, err := protocol.NewMessage("finalized", s.identity.NodeID, s.identity.NodeID, json.RawMessage(body))
	if err != nil {
		return nil, finalizedBody{}, ErrEvidence
	}
	if _, err = s.validator.Validate(probe); err != nil {
		return nil, finalizedBody{}, ErrEvidence
	}
	event, err := s.readEvent(ctx, q, r.OperationID, seq)
	if err != nil {
		return nil, finalizedBody{}, err
	}
	want, err := releaseTerminal(event.Body)
	if err != nil || event.Kind != "result" || want != terminal {
		return nil, finalizedBody{}, ErrEvidence
	}
	claims, err := readClaims(ctx, q, r.OperationID)
	if err != nil {
		return nil, finalizedBody{}, err
	}
	if len(claims) != 0 {
		return nil, finalizedBody{}, ErrEvidence
	}
	return body, p, nil
}

// ReadFinalized verifies the receipt and complete original event history without
// reserving key uses, including after the encryption budget has been exhausted.
func (s *Store) ReadFinalized(ctx context.Context, id string) (json.RawMessage, error) {
	if err := s.requireRelease(); err != nil {
		return nil, err
	}
	if !validID(id) {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	r, err := s.readRecord(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	state, err := readEventState(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	body, _, err := s.readFinalization(ctx, tx, r, state)
	if err != nil {
		return nil, err
	}
	if _, err = s.readRecoveryRecord(ctx, tx, id); err != nil {
		return nil, err
	}
	return body, nil
}

// Finalize serializes with recovery. Sealing occurs outside the business
// transaction; the final transaction repeats all session and evidence checks.
func (s *Store) Finalize(ctx context.Context, raw []byte) ([]byte, error) {
	if err := s.requireRelease(); err != nil {
		return nil, err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "finalize" || env.SessionID == nil || env.ReplyTo != nil {
		return nil, ErrInput
	}
	req, err := parseFinalize(env.Body)
	if err != nil {
		return nil, err
	}
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	defer s.freezeQuarantinedGates()
	body, needsSeal, err := s.finalize(ctx, *env.SessionID, req, nil)
	if err != nil {
		return nil, err
	}
	if needsSeal {
		sealed, err := s.sealEvent(ctx, eventBinding{OperationID: req.Operation, Digest: req.Digest, Kind: "finalized", Seq: req.Seq}, body)
		if err != nil {
			return nil, err
		}
		body, _, err = s.finalize(ctx, *env.SessionID, req, &sealed)
		if err != nil {
			return nil, err
		}
	}
	response, err := protocol.NewMessage("finalized", *env.SessionID, env.MessageID, json.RawMessage(body))
	if err != nil {
		return nil, ErrStorage
	}
	return response, nil
}

func (s *Store) finalize(ctx context.Context, session string, req finalizedBody, sealed *sealedEvent) ([]byte, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoverySession(ctx, tx, session); err != nil {
		return nil, false, err
	}
	if err = s.requireReleaseHealthy(ctx, tx); err != nil {
		return nil, false, err
	}
	r, err := s.readRecord(ctx, tx, req.Operation)
	if err != nil {
		return nil, false, err
	}
	view, err := s.readRecoveryRecord(ctx, tx, r.OperationID)
	if err != nil {
		return nil, false, err
	}
	var v struct {
		Latest json.RawMessage `json:"latest_result"`
	}
	if json.Unmarshal(view, &v) != nil {
		return nil, false, ErrEvidence
	}
	state, err := readEventState(ctx, tx, r.OperationID)
	if err != nil {
		return nil, false, err
	}
	terminal, terminalErr := releaseTerminal(v.Latest)
	if r.Digest != req.Digest || state.Blocked || !state.LatestResultSeq.Valid || uint64(state.LatestResultSeq.Int64) != req.Seq || terminalErr != nil || terminal != req.Terminal {
		if err = s.quarantineRelease(ctx, tx); err != nil {
			return nil, false, err
		}
		if err = tx.Commit(); err != nil {
			return nil, false, ErrStorage
		}
		return nil, false, ErrConflict
	}
	body, _, err := s.readFinalization(ctx, tx, r, state)
	if err == nil {
		return body, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	body, err = json.Marshal(finalizedBody{r.OperationID, r.Digest, req.Seq, terminal})
	if err != nil {
		return nil, false, ErrEvidence
	}
	if sealed == nil {
		return body, true, nil
	}
	if s.runtime != nil {
		if err = s.publishOwnership(ctx, tx, r, req.Seq, v.Latest); err != nil {
			return nil, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO finalizations(operation_id,result_event_seq,terminal_state,body_cipher,wrapped_dek,key_id,body_fingerprint) VALUES(?,?,?,?,?,?,?)`, r.OperationID, req.Seq, terminal, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint); err != nil {
		return nil, false, ErrStorage
	}
	changed, err := tx.ExecContext(ctx, `DELETE FROM local_claims WHERE operation_id=?`, r.OperationID)
	if err != nil {
		return nil, false, ErrStorage
	}
	count, err := changed.RowsAffected()
	if err != nil || count != 2 {
		return nil, false, ErrEvidence
	}
	if err = tx.Commit(); err != nil {
		return nil, false, ErrStorage
	}
	return body, false, nil
}
