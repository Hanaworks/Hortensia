package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"time"
	"unicode/utf8"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
)

type runtimeError struct {
	Category string `json:"category"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Retry    string `json:"retry_action"`
}
type runtimeResult struct {
	Operation   string          `json:"operation_id"`
	Digest      string          `json:"input_digest"`
	Seq         uint64          `json:"event_seq"`
	Phase       string          `json:"phase"`
	Outcome     string          `json:"outcome"`
	Interrupt   *string         `json:"interrupt_reason"`
	Exit        *int            `json:"exit_code"`
	Error       *runtimeError   `json:"error"`
	Observation json.RawMessage `json:"observation"`
	NotStarted  bool            `json:"not_started"`
	Output      *string         `json:"output_b64"`
	Truncated   bool            `json:"output_truncated"`
	Finished    string          `json:"finished_at"`
	Ownership   string          `json:"ownership_status"`
}

func fixedError(code string) *runtimeError {
	return &runtimeError{"execution_failed", code, "Agent execution could not complete", "query_operation"}
}

var runtimeErrorCode = regexp.MustCompile("^[a-z][a-z0-9_]{0,63}$")

func validateProcessOutcome(f ProcessOutcome) error {
	if f.ErrorCode != "" && !runtimeErrorCode.MatchString(f.ErrorCode) {
		return ErrEvidence
	}
	if !validID(f.BootID) || len(f.Output) > 1<<20 || !utf8.Valid(f.Output) {
		return ErrEvidence
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", f.FinishedAt); err != nil {
		return ErrEvidence
	}
	if f.ExitCode != nil && (*f.ExitCode < 0 || *f.ExitCode > 255) {
		return ErrEvidence
	}
	switch f.Outcome {
	case "normal_exit":
		if f.ExitCode == nil || f.InterruptReason != "" {
			return ErrEvidence
		}
	case "interrupted":
		if f.InterruptReason != "canceled" && f.InterruptReason != "deadline" && f.InterruptReason != "agent_stopping" && f.InterruptReason != "signal" {
			return ErrEvidence
		}
	case "unknown":
		if f.ExitCode != nil || f.InterruptReason != "" {
			return ErrEvidence
		}
	default:
		return ErrEvidence
	}
	return nil
}
func sameOutcome(a, b ProcessOutcome) bool {
	ar, _ := json.Marshal(a)
	br, _ := json.Marshal(b)
	return bytes.Equal(ar, br)
}
func (s *Store) enteredStart(operation string) bool {
	s.runtime.mu.Lock()
	defer s.runtime.mu.Unlock()
	return s.runtime.entered[operation]
}
func (s *Store) RecordProcessCreated(ctx context.Context, operationID string, processID int) error {
	if err := s.requireRuntime(); err != nil {
		return err
	}
	if processID <= 0 || !s.enteredStart(operationID) {
		return ErrEvidence
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	r, err := s.readRecord(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if r.BootID != s.boot || r.Phase != PhaseStartCommitted {
		return ErrFrozen
	}
	var boot, state string
	var pid sql.NullInt64
	if tx.QueryRowContext(ctx, `SELECT owner_boot,spawn_state,process_id FROM process_facts WHERE operation_id=?`, operationID).Scan(&boot, &state, &pid) != nil {
		return ErrEvidence
	}
	if boot != s.boot {
		return ErrFrozen
	}
	if state == "created" && pid.Valid && pid.Int64 == int64(processID) {
		return nil
	}
	if state != "not_attempted" {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE process_facts SET spawn_state='created',process_id=? WHERE operation_id=?`, processID, operationID); err != nil {
		return ErrStorage
	}
	if tx.Commit() != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) RecordProcessExit(ctx context.Context, operationID string, f ProcessOutcome) error {
	if err := s.requireRuntime(); err != nil {
		return err
	}
	if err := validateProcessOutcome(f); err != nil {
		return err
	}
	r, err := s.Get(ctx, operationID)
	if err != nil {
		return err
	}
	var old ProcessOutcome
	if err = s.readDocument(ctx, s.db, r, "process", 0, &old); err == nil {
		if sameOutcome(old, f) {
			return nil
		}
		return s.quarantineRuntimeConflict(ctx, operationID)
	} else if err != ErrNotFound {
		return err
	}
	if f.BootID != s.boot || r.BootID != s.boot || !s.enteredStart(operationID) {
		return ErrFrozen
	}
	sealed, err := s.sealDocument(ctx, r, "process", 0, f)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	var boot, state, cancel string
	if tx.QueryRowContext(ctx, `SELECT owner_boot,spawn_state,cancel_reason FROM process_facts WHERE operation_id=?`, operationID).Scan(&boot, &state, &cancel) != nil {
		return ErrEvidence
	}
	if boot != s.boot || r.Phase != PhaseStartCommitted {
		return ErrFrozen
	}
	if state == "exited" {
		if err = s.readDocument(ctx, tx, r, "process", 0, &old); err != nil {
			return err
		}
		if sameOutcome(old, f) {
			return nil
		}
		return ErrConflict
	}
	if state != "created" {
		return ErrEvidence
	}
	if cancel != "" && f.Outcome != "interrupted" && f.Outcome != "unknown" {
		return ErrConflict
	}
	if err = s.insertDocument(ctx, tx, r, "process", 0, sealed); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE process_facts SET spawn_state='exited' WHERE operation_id=?`, operationID); err != nil {
		return ErrStorage
	}
	if tx.Commit() != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) RecordProcessOutcome(ctx context.Context, operationID string, f ProcessOutcome, e ObservationEvidence) (Event, error) {
	if err := s.requireRuntime(); err != nil {
		return Event{}, err
	}
	// Persist exit independently before observation/result work. A repeated call
	// must carry exactly the previously stored process bytes, even after restart.
	if err := s.RecordProcessExit(ctx, operationID, f); err != nil {
		return Event{}, err
	}
	return s.writeRuntimeResult(ctx, operationID, "", nil, &f, e)
}
func (s *Store) RecordPreparationFailure(ctx context.Context, operationID string, f Failure) (Event, error) {
	if err := s.requireRuntime(); err != nil {
		return Event{}, err
	}
	r, err := s.Get(ctx, operationID)
	if err != nil {
		return Event{}, err
	}
	if r.BootID != s.boot || f.BootID != s.boot {
		return Event{}, ErrFrozen
	}
	if r.Phase == PhaseStartCommitted {
		return Event{}, ErrConflict
	}
	return s.writeRuntimeResult(ctx, operationID, "", &f, nil, ObservationEvidence{})
}
func (s *Store) RecordStartFailure(ctx context.Context, operationID string, f Failure) (Event, error) {
	if err := s.requireRuntime(); err != nil {
		return Event{}, err
	}
	if f.BootID != s.boot {
		return Event{}, ErrFrozen
	}
	s.mu.Lock()
	g := s.gates[operationID]
	s.mu.Unlock()
	if g == nil {
		return Event{}, ErrEvidence
	}
	s.runtime.mu.Lock()
	committed := s.runtime.committed[operationID]
	s.runtime.mu.Unlock()
	if !committed {
		return Event{}, ErrEvidence
	}
	state := g.State()
	// An entered callback must explicitly report a synchronous Start failure.
	// If it never entered, a final terminal gate state proves no later callback.
	if s.enteredStart(operationID) {
		if f.Code != "spawn_failed" {
			return Event{}, ErrEvidence
		}
	} else if state != startgate.Canceled && state != startgate.Invalidated && state != startgate.Expired && state != startgate.Denied {
		return Event{}, ErrEvidence
	}
	return s.writeRuntimeResult(ctx, operationID, "", &f, nil, ObservationEvidence{})
}
func (s *Store) CancelExecution(ctx context.Context, raw []byte) (CancelDecision, error) {
	if err := s.requireRuntime(); err != nil {
		return CancelDecision{}, err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "cancel" || env.SessionID == nil {
		return CancelDecision{}, ErrInput
	}
	var b struct {
		Operation string `json:"operation_id"`
		Digest    string `json:"input_digest"`
	}
	if json.Unmarshal(env.Body, &b) != nil {
		return CancelDecision{}, ErrInput
	}
	// Serialize with binding/scan replacement, but never hold SQLite while
	// waiting for Gate. The initial snapshot rejects bad inputs before any Gate
	// effect; the mutation transaction repeats every durable predicate afterward.
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	preflight, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CancelDecision{}, ErrStorage
	}
	_, preState, err := s.validateRuntimeCancel(ctx, preflight, *env.SessionID, b.Operation, b.Digest)
	if err != nil {
		preflight.Rollback()
		return CancelDecision{}, err
	}
	if preState.LatestResultSeq.Valid {
		event, readErr := s.readEvent(ctx, preflight, b.Operation, uint64(preState.LatestResultSeq.Int64))
		rollbackErr := preflight.Rollback()
		if readErr != nil {
			return CancelDecision{}, readErr
		}
		if rollbackErr != nil {
			return CancelDecision{}, ErrStorage
		}
		return CancelDecision{"reported", &event}, nil
	}
	if err = preflight.Rollback(); err != nil {
		return CancelDecision{}, ErrStorage
	}
	// Never wait for a Gate while holding SQLite. Cancel either stops entry or
	// returns after the already entered short callback has registered its child.
	s.mu.Lock()
	g := s.gates[b.Operation]
	s.mu.Unlock()
	if g != nil {
		_ = g.Cancel()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CancelDecision{}, ErrStorage
	}
	defer tx.Rollback()
	r, state, err := s.validateRuntimeCancel(ctx, tx, *env.SessionID, b.Operation, b.Digest)
	if err != nil {
		return CancelDecision{}, err
	}
	if state.LatestResultSeq.Valid {
		event, err := s.readEvent(ctx, tx, b.Operation, uint64(state.LatestResultSeq.Int64))
		return CancelDecision{"reported", &event}, err
	}
	s.runtime.mu.Lock()
	commitFailed := s.runtime.commitFailed[b.Operation]
	s.runtime.mu.Unlock()
	if commitFailed {
		return CancelDecision{Disposition: "interrupt_required"}, nil
	}
	started := r.Phase == PhaseStartCommitted
	if started {
		if _, err = tx.ExecContext(ctx, `UPDATE process_facts SET cancel_reason='canceled' WHERE operation_id=? AND spawn_state<>'exited'`, b.Operation); err != nil {
			return CancelDecision{}, ErrStorage
		}
	}
	if tx.Commit() != nil {
		return CancelDecision{}, ErrStorage
	}
	if started {
		if r.BootID != s.boot || g == nil || s.enteredStart(b.Operation) {
			return CancelDecision{Disposition: "interrupt_required"}, nil
		}
		event, err := s.RecordStartFailure(ctx, b.Operation, Failure{Code: "canceled_before_start", BootID: s.boot, session: *env.SessionID})
		return CancelDecision{"not_started", &event}, err
	}
	// Reopened preparing/ready has no START_COMMITTED or process row: explicit
	// current recovery cancellation may safely end it without resuming prepare.
	event, err := s.writeRuntimeResult(ctx, b.Operation, "", &Failure{Code: "canceled_before_start", BootID: s.boot, session: *env.SessionID}, nil, ObservationEvidence{})
	return CancelDecision{"not_started", &event}, err
}
func (s *Store) BeginObservationQuery(ctx context.Context, operationID, queryID string, limit int) (ObservationQuery, error) {
	if err := s.requireRuntime(); err != nil {
		return ObservationQuery{}, err
	}
	if !validID(queryID) || !validID(operationID) || limit < 1 || limit > 200 {
		return ObservationQuery{}, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObservationQuery{}, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoveryWriter(ctx, tx); err != nil {
		return ObservationQuery{}, err
	}
	query, err := s.readObservationQuery(ctx, tx, operationID, queryID, limit)
	if err == nil {
		return query, nil
	}
	if err != ErrNotFound {
		return ObservationQuery{}, err
	}
	r, err := s.readRecord(ctx, tx, operationID)
	if err != nil {
		return ObservationQuery{}, err
	}
	state, err := readEventState(ctx, tx, operationID)
	if err != nil {
		return ObservationQuery{}, err
	}
	if err = s.allowObservationUpdate(ctx, tx, r, state); err != nil {
		return ObservationQuery{}, err
	}
	now := s.runtime.now()
	var active int
	if tx.QueryRowContext(ctx, "SELECT count(*) FROM observation_queries WHERE operation_id=? AND owner_boot=? AND result_seq IS NULL AND created_at>?", operationID, s.boot, now.Add(-5*time.Minute).UnixMilli()).Scan(&active) != nil {
		return ObservationQuery{}, ErrStorage
	}
	if active != 0 {
		return ObservationQuery{}, ErrScanBusy
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO observation_queries(query_id,operation_id,owner_boot,page_limit,created_at) VALUES(?,?,?,?,?)`, queryID, operationID, s.boot, limit, now.UnixMilli()); err != nil {
		return ObservationQuery{}, ErrStorage
	}
	if tx.Commit() != nil {
		return ObservationQuery{}, ErrStorage
	}
	return ObservationQuery{QueryID: queryID, OperationID: operationID, BootID: s.boot, Limit: limit, CreatedAt: now, Disposition: "collect"}, nil
}
func (s *Store) readObservationQuery(ctx context.Context, q querier, operationID, queryID string, limit int) (ObservationQuery, error) {
	var query ObservationQuery
	var seq sql.NullInt64
	var created int64
	err := q.QueryRowContext(ctx, `SELECT query_id,operation_id,owner_boot,page_limit,created_at,result_seq FROM observation_queries WHERE query_id=?`, queryID).Scan(&query.QueryID, &query.OperationID, &query.BootID, &query.Limit, &created, &seq)
	if err == sql.ErrNoRows {
		return query, ErrNotFound
	}
	if err != nil {
		return query, ErrStorage
	}
	if query.OperationID != operationID || (limit != 0 && query.Limit != limit) {
		return query, ErrConflict
	}
	query.CreatedAt = time.UnixMilli(created)
	if query.BootID != s.boot || s.runtime.now().Sub(query.CreatedAt) >= 5*time.Minute {
		return query, ErrScanExpired
	}
	query.Disposition = "collecting"
	if seq.Valid {
		query.ResultSeq = uint64(seq.Int64)
		query.Disposition = "replay"
	}
	return query, nil
}
func (s *Store) allowObservationUpdate(ctx context.Context, q querier, r Record, state eventState) error {
	if state.Blocked || !state.LatestResultSeq.Valid {
		return ErrConflict
	}
	var finalized int
	if q.QueryRowContext(ctx, `SELECT count(*) FROM finalizations WHERE operation_id=?`, r.OperationID).Scan(&finalized) != nil {
		return ErrStorage
	}
	if finalized != 0 {
		return ErrConflict
	}
	event, err := s.readEvent(ctx, q, r.OperationID, uint64(state.LatestResultSeq.Int64))
	if err != nil {
		return err
	}
	var result runtimeResult
	if json.Unmarshal(event.Body, &result) != nil {
		return ErrEvidence
	}
	var obs struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(result.Observation, &obs)
	if result.Outcome != "normal_exit" || result.Ownership == "drifted" || obs.Status == "complete" {
		return ErrConflict
	}
	return nil
}
func (s *Store) RecordObservationUpdate(ctx context.Context, operationID, queryID string, e ObservationEvidence) (Event, error) {
	if err := s.requireRuntime(); err != nil {
		return Event{}, err
	}
	return s.writeRuntimeResult(ctx, operationID, queryID, nil, nil, e)
}

func (s *Store) writeRuntimeResult(ctx context.Context, operation, query string, failure *Failure, fact *ProcessOutcome, e ObservationEvidence) (Event, error) {
	event, candidate, err := s.prepareRuntimeResult(ctx, operation, query, failure, fact, e, nil, nil)
	if err != nil || event.Kind != "seal" {
		return event, err
	}
	binding := eventBinding{event.OperationID, event.Digest, "result", event.Seq}
	sealed, err := s.sealEvent(ctx, binding, event.Body)
	if err != nil {
		return Event{}, err
	}
	var candidateSeal *sealedEvent
	if candidate != nil {
		r, err := s.Get(ctx, operation)
		if err != nil {
			return Event{}, err
		}
		seal, err := s.sealDocument(ctx, r, "candidate", event.Seq, *candidate)
		if err != nil {
			return Event{}, err
		}
		candidateSeal = &seal
	}
	result, _, err := s.prepareRuntimeResult(ctx, operation, query, failure, fact, e, &sealed, candidateSeal)
	return result, err
}
func (s *Store) prepareRuntimeResult(ctx context.Context, operation, query string, failure *Failure, fact *ProcessOutcome, e ObservationEvidence, sealed, candidateSeal *sealedEvent) (Event, *ownershipCandidate, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, nil, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoveryWriter(ctx, tx); err != nil {
		return Event{}, nil, err
	}
	r, err := s.readRecord(ctx, tx, operation)
	if err != nil {
		return Event{}, nil, err
	}
	state, err := readEventState(ctx, tx, operation)
	if err != nil {
		return Event{}, nil, err
	}
	if state.Blocked {
		return Event{}, nil, ErrFrozen
	}
	if query != "" {
		q, err := s.readObservationQuery(ctx, tx, operation, query, 0)
		if err != nil {
			return Event{}, nil, err
		}
		if q.ResultSeq != 0 {
			event, err := s.readEvent(ctx, tx, operation, q.ResultSeq)
			return event, nil, err
		}
		if err = s.allowObservationUpdate(ctx, tx, r, state); err != nil {
			return Event{}, nil, err
		}
	} else if state.LatestResultSeq.Valid {
		// Idempotence must never accept a conflicting process outcome.
		event, err := s.readEvent(ctx, tx, operation, uint64(state.LatestResultSeq.Int64))
		if err != nil {
			return Event{}, nil, err
		}
		var result runtimeResult
		if json.Unmarshal(event.Body, &result) != nil {
			return Event{}, nil, ErrEvidence
		}
		if failure != nil {
			if result.Outcome != "not_started" || result.Error == nil || result.Error.Code != failure.Code {
				return Event{}, nil, ErrConflict
			}
		} else if fact != nil {
			old, err := s.readProcessOutcome(ctx, tx, r)
			if err != nil {
				return Event{}, nil, err
			}
			if !sameOutcome(old, *fact) {
				return Event{}, nil, ErrConflict
			}
		} else {
			return Event{}, nil, ErrConflict
		}
		return event, nil, nil
	}
	if err = requireEventClaims(ctx, tx, r); err != nil {
		return Event{}, nil, err
	}
	result := runtimeResult{Operation: r.OperationID, Digest: r.Digest, Seq: state.LastSeq + 1, Phase: "result", Finished: timestamp(s.runtime.now().UnixMilli())}
	var candidate *ownershipCandidate
	if failure != nil {
		s.runtime.mu.Lock()
		commitFailed := s.runtime.commitFailed[operation]
		s.runtime.mu.Unlock()
		if commitFailed {
			return Event{}, nil, ErrEvidence
		}
		if failure.session != "" {
			if err = s.requireRecoverySession(ctx, tx, failure.session); err != nil {
				return Event{}, nil, err
			}
		}
		if failure.Code == "" || failure.BootID != s.boot {
			return Event{}, nil, ErrEvidence
		}
		if r.Phase == PhaseStartCommitted {
			var boot, spawn string
			if tx.QueryRowContext(ctx, `SELECT owner_boot,spawn_state FROM process_facts WHERE operation_id=?`, operation).Scan(&boot, &spawn) != nil {
				return Event{}, nil, ErrEvidence
			}
			if boot != s.boot || r.BootID != s.boot || spawn != "not_attempted" || state.LastSeq != 1 {
				return Event{}, nil, ErrEvidence
			}
		} else if state.LastSeq != 0 {
			return Event{}, nil, ErrConflict
		}
		result.Outcome, result.Ownership, result.NotStarted, result.Error = "not_started", "not_checked", true, fixedError(failure.Code)
	} else {
		stored, err := s.readProcessOutcome(ctx, tx, r)
		if err != nil {
			return Event{}, nil, err
		}
		if fact != nil && !sameOutcome(stored, *fact) {
			return Event{}, nil, ErrConflict
		}
		if query != "" && stored.Outcome != "normal_exit" {
			return Event{}, nil, ErrConflict
		}
		if r.Phase != PhaseStartCommitted || state.LastSeq < 1 {
			return Event{}, nil, ErrConflict
		}
		if err = s.requireStartedEvent(ctx, tx, r); err != nil {
			return Event{}, nil, err
		}
		if err = validateProcessOutcome(stored); err != nil {
			return Event{}, nil, err
		}
		var c ownershipCandidate
		if len(e.Observation) == 0 && stored.Outcome != "normal_exit" {
			var prep PreparationEvidence
			if err = s.readDocument(ctx, tx, r, "preparation", 0, &prep); err != nil {
				return Event{}, nil, err
			}
			c = ownershipCandidate{PreRevision: prep.OwnershipRevision, Post: e.Post, Replacements: e.Replacements, Status: "unknown"}
			if len(e.Conflicts) > 0 {
				c.Status = "drifted"
			}
		} else {
			c, err = s.ownership(ctx, tx, r, e)
			if err != nil {
				return Event{}, nil, err
			}
		}

		result.Outcome, result.Exit, result.Finished, result.Truncated = stored.Outcome, stored.ExitCode, stored.FinishedAt, stored.OutputTruncated
		output := base64.StdEncoding.EncodeToString(stored.Output)
		result.Output = &output
		if stored.Outcome == "normal_exit" && stored.ExitCode != nil && *stored.ExitCode != 0 {
			code := stored.ErrorCode
			if code == "" {
				code = "compose_exit_nonzero"
			}
			result.Error = fixedError(code)
		}
		result.Observation = e.Observation
		result.Ownership = c.Status
		if stored.Outcome != "normal_exit" {
			if result.Ownership != "drifted" {
				result.Ownership = "unknown"
			}
			c.Status = result.Ownership
			result.Error = fixedError(stored.ErrorCode)
			if stored.ErrorCode == "" {
				result.Error = fixedError(stored.Outcome)
			}
			if stored.InterruptReason != "" {
				reason := stored.InterruptReason
				result.Interrupt = &reason
			}
		}
		candidate = &c
	}
	if sealed != nil && failure != nil {
		oldBody, openErr := s.openEvent(eventBinding{operation, r.Digest, "result", result.Seq}, *sealed)
		if openErr != nil {
			return Event{}, nil, openErr
		}
		var old runtimeResult
		if json.Unmarshal(oldBody, &old) != nil {
			return Event{}, nil, ErrEvidence
		}
		result.Finished = old.Finished
	}
	body, err := json.Marshal(result)
	if err != nil {
		return Event{}, nil, ErrInput
	}
	probe, err := protocol.NewMessage("result", s.identity.NodeID, "", json.RawMessage(body))
	if err != nil {
		return Event{}, nil, ErrInput
	}
	if _, err = s.validator.Validate(probe); err != nil {
		return Event{}, nil, ErrEvidence
	}
	if len(body) > maxEventBodyBytes {
		return Event{}, nil, ErrInput
	}
	event := Event{OperationID: operation, Digest: r.Digest, Kind: "seal", Seq: result.Seq, Body: body}
	if sealed == nil {
		return event, candidate, nil
	}
	// Recheck sealing input: a racing writer must not store ciphertext for a
	// different newly computed sequence or process/observation body.
	opened, err := s.openEvent(eventBinding{operation, r.Digest, "result", result.Seq}, *sealed)
	if err != nil || !bytes.Equal(opened, body) {
		return Event{}, nil, ErrConflict
	}
	if candidate != nil {
		if candidate.Status == "drifted" {
			if _, err = tx.ExecContext(ctx, "UPDATE project_bindings SET status='drifted' WHERE endpoint_id=? AND project_name=?", r.EndpointID, r.ProjectName); err != nil {
				return Event{}, nil, ErrStorage
			}
		}
		if candidateSeal == nil {
			return Event{}, nil, ErrEvidence
		}
		candidateBody, openErr := s.openEvent(eventBinding{operation, r.Digest, "runtime-candidate", result.Seq}, *candidateSeal)
		expectedBody, _ := json.Marshal(candidate)
		if openErr != nil || !bytes.Equal(candidateBody, expectedBody) {
			return Event{}, nil, ErrConflict
		}
		if err = s.insertDocument(ctx, tx, r, "candidate", result.Seq, *candidateSeal); err != nil {
			return Event{}, nil, err
		}
	}
	if err = s.insertEvent(ctx, tx, eventBinding{operation, r.Digest, "result", result.Seq}, *sealed); err != nil {
		return Event{}, nil, err
	}
	if failure != nil && r.Phase == PhaseStartCommitted {
		if _, err = tx.ExecContext(ctx, `UPDATE process_facts SET spawn_state='not_started' WHERE operation_id=?`, operation); err != nil {
			return Event{}, nil, ErrStorage
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE event_state SET last_seq=?,latest_result_seq=? WHERE operation_id=?`, result.Seq, result.Seq, operation); err != nil {
		return Event{}, nil, ErrStorage
	}
	if query != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE observation_queries SET result_seq=? WHERE query_id=? AND result_seq IS NULL`, result.Seq, query); err != nil {
			return Event{}, nil, ErrStorage
		}
	}
	if tx.Commit() != nil {
		return Event{}, nil, ErrStorage
	}
	event.Kind = "result"
	return event, candidate, nil
}

func (s *Store) validateRuntimeHistoryEvent(ctx context.Context, q querier, r Record, event Event, previous *Event) error {
	var result runtimeResult
	if json.Unmarshal(event.Body, &result) != nil {
		return ErrEvidence
	}
	if result.Outcome == "not_started" {
		if previous != nil {
			return ErrEvidence
		}
		if r.Phase == PhaseStartCommitted {
			var state, boot string
			if q.QueryRowContext(ctx, `SELECT spawn_state,owner_boot FROM process_facts WHERE operation_id=?`, r.OperationID).Scan(&state, &boot) != nil || state != "not_started" || boot != r.BootID || event.Seq != 2 {
				return ErrEvidence
			}
		} else if event.Seq != 1 {
			return ErrEvidence
		}
		return nil
	}
	fact, err := s.readProcessOutcome(ctx, q, r)
	if err != nil {
		return err
	}
	if fact.BootID != r.BootID || result.Outcome != fact.Outcome || result.Finished != fact.FinishedAt || result.Truncated != fact.OutputTruncated || result.Output == nil || *result.Output != base64.StdEncoding.EncodeToString(fact.Output) {
		return ErrEvidence
	}
	if (result.Exit == nil) != (fact.ExitCode == nil) || (result.Exit != nil && *result.Exit != *fact.ExitCode) {
		return ErrEvidence
	}
	if (result.Interrupt == nil) != (fact.InterruptReason == "") || (result.Interrupt != nil && *result.Interrupt != fact.InterruptReason) {
		return ErrEvidence
	}
	var candidate ownershipCandidate
	if err := s.readDocument(ctx, q, r, "candidate", event.Seq, &candidate); err != nil {
		return err
	}
	if candidate.Status != result.Ownership {
		return ErrEvidence
	}
	if previous != nil {
		var old runtimeResult
		var obs struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(previous.Body, &old) != nil {
			return ErrEvidence
		}
		json.Unmarshal(old.Observation, &obs)
		if old.Outcome != "normal_exit" || old.Ownership == "drifted" || obs.Status == "complete" {
			return ErrEvidence
		}
	}
	return nil
}

// validateRuntimeCancel is shared by both snapshots. It does not acquire Gate
// locks or reserve crypto quota, and can run under the caller's SQLite lock.
func (s *Store) validateRuntimeCancel(ctx context.Context, q querier, session, operation, digest string) (Record, eventState, error) {
	if err := s.requireRecoverySession(ctx, q, session); err != nil {
		return Record{}, eventState{}, err
	}
	r, err := s.readRecord(ctx, q, operation)
	if err != nil {
		return r, eventState{}, err
	}
	if r.Digest != digest {
		return r, eventState{}, ErrConflict
	}
	state, err := readEventState(ctx, q, operation)
	if err != nil {
		return r, state, err
	}
	if state.Blocked {
		return r, state, ErrFrozen
	}
	if !state.LatestResultSeq.Valid {
		if err = requireEventClaims(ctx, q, r); err != nil {
			return r, state, err
		}
	}
	return r, state, nil
}
