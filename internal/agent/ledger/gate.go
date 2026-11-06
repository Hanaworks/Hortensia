package ledger

import (
	"context"
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
	defer s.mu.Unlock()
	if s.closing {
		return nil, ErrStorage
	}
	if !validID(request.Session) || !validID(request.Operation) || !validID(request.Nonce) || !validID(request.MessageID) || !validDigest(request.Digest) {
		return nil, ErrInput
	}
	if _, exists := s.gates[request.Operation]; exists {
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
	result, err := tx.ExecContext(ctx, `UPDATE executions SET phase=?,session_id=?,start_nonce=?,permit_id=?,auth_rev=?,reply_to=?,started_at=? WHERE operation_id=? AND owner_boot_id=? AND phase=?`,
		PhaseStartCommitted, permit.Session, permit.Nonce, permit.PermitID, permit.AuthRev, permit.ReplyTo, time.Now().UnixMilli(), permit.Operation, s.boot, PhaseReady)
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
