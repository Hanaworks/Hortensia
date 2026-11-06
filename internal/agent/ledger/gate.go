package ledger

import (
	"context"
	"encoding/json"
	"time"

	"docker-manager/internal/startgate"
)

// NewGate binds one local request window to this boot's prepared record. A
// successfully constructed gate stays registered even after failure or cancel;
// constructing another gate must never turn uncertainty into permission.
// The context governs database work only. Business cancellation and connection
// loss must still be delivered explicitly to Gate.Cancel or Gate.Invalidate.
// Start must be short and must not reenter Gate or call Store.Close; Close waits
// for an already entered Start callback to return. Start may query Get/Claims.
func (s *Store) NewGate(ctx context.Context, request startgate.Request, now func() time.Time, start func(startgate.Permit)) (*startgate.Gate, error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, ErrStorage
	}
	_, exists := s.gates[request.Operation]
	s.mu.Unlock()
	if !validID(request.Session) || !validID(request.Operation) || !validID(request.Nonce) || !validID(request.MessageID) || !validDigest(request.Digest) {
		return nil, ErrInput
	}
	if exists {
		return nil, ErrGateExists
	}
	record, err := s.readRecord(ctx, s.db, request.Operation)
	if err != nil {
		return nil, err
	}
	if record.BootID != s.boot {
		return nil, ErrFrozen
	}
	if record.PreparedSession != request.Session || record.Digest != request.Digest {
		return nil, ErrConflict
	}
	if record.Phase == PhaseStartCommitted {
		return nil, ErrAlreadyCommitted
	}
	if record.Phase != PhaseReady {
		return nil, ErrConflict
	}
	if s.journal != nil {
		state, err := readEventState(ctx, s.db, request.Operation)
		if err != nil {
			return nil, err
		}
		if state.Blocked || state.LatestResultSeq.Valid || state.LastSeq != 0 {
			return nil, ErrConflict
		}
	}
	gate, err := startgate.New(request, startgate.Hooks{
		Now: now,
		CommitStart: func(permit startgate.Permit) error {
			return s.commitStartExpected(ctx, permit, &record)
		},
		Start: start,
	})
	if err != nil {
		return nil, err
	}
	// Do not hold mu while querying SQLite: readers decrypt within a database
	// transaction and briefly take mu to copy keys. Recheck registration and
	// Close here so concurrent constructors still cannot grant two gates.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, ErrStorage
	}
	if _, exists := s.gates[request.Operation]; exists {
		return nil, ErrGateExists
	}
	s.gates[request.Operation] = gate
	return gate, nil
}

// commitStart returns nil only for this call's first durable transition. A
// matching existing fact is evidence of a previous commitment, never a fresh
// opportunity to invoke Start. ReplyTo is correlation, not permit identity.
func (s *Store) commitStart(ctx context.Context, permit startgate.Permit) error {
	return s.commitStartExpected(ctx, permit, nil)
}

func (s *Store) commitStartExpected(ctx context.Context, permit startgate.Permit, expected *Record) error {
	if !validID(permit.Session) || !validID(permit.Operation) || !validID(permit.Nonce) || !validID(permit.PermitID) || !validID(permit.ReplyTo) || !validDigest(permit.Digest) || permit.AuthRev > 9007199254740991 {
		return ErrInput
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return ErrStorage
	}
	startedAt := time.Now().UnixMilli()
	var started sealedEvent
	var binding eventBinding
	if s.journal != nil {
		binding = eventBinding{OperationID: permit.Operation, Digest: permit.Digest, Kind: "started", Seq: 1}
		body, err := json.Marshal(struct {
			Operation string `json:"operation_id"`
			Digest    string `json:"input_digest"`
			Seq       uint64 `json:"event_seq"`
			Phase     string `json:"phase"`
			PermitID  string `json:"permit_id"`
			StartedAt string `json:"started_at"`
		}{permit.Operation, permit.Digest, 1, PhaseStartCommitted, permit.PermitID, timestamp(startedAt)})
		if err != nil {
			return ErrInput
		}
		started, err = s.sealEvent(ctx, binding, body)
		if err != nil {
			return err
		}
	}
	// databaseURI selects BEGIN IMMEDIATE for every sql.Tx. No pre-read can
	// grant execution rights; all durable predicates are checked under its lock.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	record, err := s.readRecord(ctx, tx, permit.Operation)
	if err != nil {
		return err
	}
	if record.BootID != s.boot {
		return ErrFrozen
	}
	if record.PreparedSession != permit.Session || record.Digest != permit.Digest {
		return ErrConflict
	}
	if expected != nil && !samePreparedRecord(record, *expected) {
		return ErrConflict
	}
	claims, err := readClaims(ctx, tx, permit.Operation)
	if err != nil {
		return err
	}
	// readClaims has a fixed ordering. Requiring this exact pair also rejects
	// extra claims or a key silently reassigned since preparation.
	if len(claims) != 2 || claims[0] != (Claim{"project", projectResourceID(record.EndpointID, record.ProjectName)}) || claims[1] != (Claim{"stack", record.StackID}) {
		return ErrConflict
	}
	if record.Phase == PhaseStartCommitted {
		if record.Permit != nil && samePermit(*record.Permit, permit) {
			return ErrAlreadyCommitted
		}
		return ErrConflict
	}
	if record.Phase != PhaseReady {
		return ErrConflict
	}
	if s.journal != nil {
		state, err := readEventState(ctx, tx, permit.Operation)
		if err != nil {
			return err
		}
		if state.Blocked || state.LatestResultSeq.Valid || state.LastSeq != 0 {
			return ErrConflict
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE executions SET phase=?,session_id=?,start_nonce=?,permit_id=?,auth_rev=?,reply_to=?,started_at=? WHERE operation_id=? AND owner_boot_id=? AND phase=?`,
		PhaseStartCommitted, permit.Session, permit.Nonce, permit.PermitID, permit.AuthRev, permit.ReplyTo, startedAt, permit.Operation, s.boot, PhaseReady)
	if err != nil {
		return ErrStorage
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ErrStorage
	}
	if changed != 1 {
		return ErrConflict
	}
	if s.journal != nil {
		if err := insertEvent(ctx, tx, binding, started); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE event_state SET last_seq=1 WHERE operation_id=?`, permit.Operation); err != nil {
			return ErrStorage
		}
	}
	if err := tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}

func samePreparedRecord(a, b Record) bool {
	return a.Identity == b.Identity && a.OperationID == b.OperationID && a.StackID == b.StackID && a.VersionID == b.VersionID &&
		a.Digest == b.Digest && a.LocalFingerprint == b.LocalFingerprint && a.ProjectName == b.ProjectName && a.EndpointID == b.EndpointID &&
		a.PolicyRevision == b.PolicyRevision && a.PreparedSession == b.PreparedSession && a.BootID == b.BootID && a.CreatedAt == b.CreatedAt
}

func samePermit(a, b startgate.Permit) bool {
	return a.Session == b.Session && a.Operation == b.Operation && a.Digest == b.Digest && a.Nonce == b.Nonce && a.PermitID == b.PermitID && a.AuthRev == b.AuthRev
}
