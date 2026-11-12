package operations

import (
	"context"
	"database/sql"
	"docker-manager/internal/protocol"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

func deliveryBinding(b Binding, d Delivery, boot, round string) blobBinding {
	metadata, _ := json.Marshal([]any{b.LedgerID, b.SessionID, boot, round, d.MessageID, d.Kind, d.Generation})
	return blobBinding{OperationID: d.OperationID, NodeID: b.NodeID, Digest: string(metadata), Kind: "runtime_delivery"}
}

// NextNodeDelivery never converts a successful write into a business receipt.
// The transaction below reserves a retry window before returning the raw bytes.
func (s *Store) NextNodeDelivery(ctx context.Context, b Binding, now time.Time) (*Delivery, error) {
	if s.version != 5 {
		return nil, ErrSchema
	}
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if e := s.requireBinding(ctx, s.db, b, true); e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT o.operation_id,x.kind,x.generation FROM outbox x JOIN operations o USING(operation_id) WHERE o.node_id=? AND x.delivery_state='pending' ORDER BY CASE x.kind WHEN 'cancel' THEN 0 WHEN 'finalize' THEN 1 ELSE 2 END,o.created_at LIMIT 32`, b.NodeID)
	if e != nil {
		return nil, ErrStorage
	}
	var candidates []Delivery
	for rows.Next() {
		var d Delivery
		if e = rows.Scan(&d.OperationID, &d.Kind, &d.Generation); e != nil {
			rows.Close()
			return nil, ErrStorage
		}
		candidates = append(candidates, d)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, ErrStorage
	}
	// A held normal exit can be repaired only by a read-only observation. Its
	// result sequence is the stable generation, so retry cannot execute Compose.
	ids, e := operationIDs(ctx, s.db, b.NodeID)
	if e != nil {
		return nil, e
	}
	for _, id := range ids {
		op, e := s.Get(ctx, id)
		if e != nil {
			return nil, e
		}
		if op.ReleaseStatus == "released" && op.LatestResultSeq > 0 {
			needed, e := s.needsRecoveryFinalization(ctx, s.db, b, op)
			if e != nil {
				return nil, e
			}
			if needed {
				candidates = append(candidates, Delivery{OperationID: id, Kind: "finalize", Generation: 1})
			}
		}
		if op.LatestResultSeq > 0 && !op.Blocked && op.ReleaseStatus == "held" {
			event, e := s.ReadEvent(ctx, id, op.LatestResultSeq)
			if e != nil {
				return nil, e
			}
			p, _ := parseEvent(event.Body)
			clear(event.Body)
			if observationRepairable(p) {
				candidates = append(candidates, Delivery{OperationID: id, Kind: "observe", Generation: op.LatestResultSeq})
			}
		}
	}
	for _, d := range candidates {
		round, e := s.deliveryRound(ctx, s.db, b)
		if e != nil {
			return nil, e
		}
		permitted, e := s.deliveryEligible(ctx, s.db, b, d)
		if e != nil {
			return nil, e
		}
		if !permitted {
			continue
		}
		var attempts uint64
		var next int64
		var storedNode, storedLedger, failureCode string
		var blob sealedBlob
		e = s.db.QueryRowContext(ctx, `SELECT message_id,body_cipher,wrapped_dek,key_id,fingerprint,attempts,next_attempt,node_id,ledger_id,failure_code FROM runtime_deliveries WHERE operation_id=? AND kind=? AND generation=? AND session_id=? AND manager_boot=? AND round_id=?`, d.OperationID, d.Kind, d.Generation, b.SessionID, s.boot, round).Scan(&d.MessageID, &blob.Cipher, &blob.WrappedDEK, &blob.KeyID, &blob.Fingerprint, &attempts, &next, &storedNode, &storedLedger, &failureCode)
		existing := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, ErrStorage
		}
		if existing && (storedNode != b.NodeID || storedLedger != b.LedgerID) {
			return nil, ErrEvidence
		}
		if existing && d.Kind == "observe" && (failureCode == "cursor_expired" || failureCode == "observation_failed") {
			continue
		}
		if existing && now.UnixMilli() < next {
			continue
		}
		if existing {
			d.Raw, e = s.unseal(deliveryBinding(b, d, s.boot, round), blob)
			if e != nil {
				return nil, e
			}
		} else {
			op, e := s.Get(ctx, d.OperationID)
			if e != nil {
				return nil, e
			}
			switch d.Kind {
			case "prepare":
				payload, e := s.ReadPayload(ctx, op.OperationID)
				if e != nil {
					return nil, e
				}
				d.Raw, e = protocol.NewMessage("prepare", b.SessionID, "", prepareFields{op.OperationID, op.Digest, op.NodeID, op.StackID, op.VersionID, base64.StdEncoding.EncodeToString(payload)})
				clear(payload)
				if e != nil {
					return nil, ErrStorage
				}
			case "cancel":
				d.Raw, e = protocol.NewMessage("cancel", b.SessionID, "", map[string]any{"operation_id": op.OperationID, "input_digest": op.Digest})
				if e != nil {
					return nil, ErrStorage
				}
			case "finalize":
				d.Raw, e = s.PrepareFinalize(ctx, b, op.OperationID)
				if errors.Is(e, ErrFrozen) || errors.Is(e, ErrRecoveryIncomplete) {
					continue
				}
				if e != nil {
					return nil, e
				}
			case "observe":
				query, e := protocol.NewID()
				if e != nil {
					return nil, ErrStorage
				}
				d.Raw, e = protocol.NewMessage("observe", b.SessionID, "", map[string]any{"query_id": query, "operation_id": op.OperationID, "resource_kind": "container", "stack_id": op.StackID, "cursor": nil, "limit": 200})
				if e != nil {
					return nil, ErrStorage
				}
			}
			env, e := s.validator.Validate(d.Raw)
			if e != nil {
				return nil, ErrEvidence
			}
			d.MessageID = env.MessageID
			blob, e = s.seal(ctx, deliveryBinding(b, d, s.boot, round), d.Raw)
			if e != nil {
				clear(d.Raw)
				return nil, e
			}
		}
		env, e := s.validator.Validate(d.Raw)
		if e != nil || env.Type != d.Kind || env.MessageID != d.MessageID || env.SessionID == nil || *env.SessionID != b.SessionID {
			clear(d.Raw)
			return nil, ErrEvidence
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			clear(d.Raw)
			return nil, ErrStorage
		}
		currentRound, roundErr := s.deliveryRound(ctx, tx, b)
		if roundErr != nil || currentRound != round {
			tx.Rollback()
			clear(d.Raw)
			if roundErr != nil {
				return nil, roundErr
			}
			continue
		}
		permitted, e = s.deliveryEligible(ctx, tx, b, d)
		if e != nil || !permitted {
			tx.Rollback()
			clear(d.Raw)
			if e != nil {
				return nil, e
			}
			continue
		}
		attempts++
		next = now.Add(deliveryDelay(attempts)).UnixMilli()
		if existing {
			_, e = tx.ExecContext(ctx, `UPDATE runtime_deliveries SET attempts=?,next_attempt=? WHERE message_id=?`, attempts, next, d.MessageID)
		} else {
			_, e = tx.ExecContext(ctx, `INSERT INTO runtime_deliveries(message_id,operation_id,node_id,ledger_id,session_id,manager_boot,round_id,kind,generation,body_cipher,wrapped_dek,key_id,fingerprint,attempts,next_attempt,failure_code) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'')`, d.MessageID, d.OperationID, b.NodeID, b.LedgerID, b.SessionID, s.boot, round, d.Kind, d.Generation, blob.Cipher, blob.WrappedDEK, blob.KeyID, blob.Fingerprint, attempts, next)
		}
		if e != nil {
			tx.Rollback()
			clear(d.Raw)
			return nil, ErrStorage
		}
		if e = tx.Commit(); e != nil {
			clear(d.Raw)
			return nil, ErrStorage
		}
		return &d, nil
	}
	return nil, nil
}

func observationRepairable(p parsedEvent) bool {
	return p.Outcome == "normal_exit" && p.Observation != nil && p.Observation.Status != "complete" && p.Ownership != "drifted"
}
func (s *Store) deliveryEligible(ctx context.Context, q querier, b Binding, d Delivery) (bool, error) {
	if e := s.requireBinding(ctx, q, b, true); e != nil {
		return false, e
	}
	op, e := s.readOperation(ctx, q, d.OperationID)
	if e != nil {
		return false, e
	}
	if !bindingMatchesOperation(b, op) {
		return false, ErrIdentity
	}
	if e = s.requireOperationEvidence(ctx, q, op); e != nil {
		return false, e
	}
	if op.Blocked {
		return false, nil
	}
	historical := op.ReleaseStatus == "released"
	if historical {
		if d.Kind != "finalize" {
			return false, nil
		}
		needed, e := s.needsRecoveryFinalization(ctx, q, b, op)
		if e != nil || !needed {
			return false, e
		}
	}
	if d.Kind == "observe" {
		if op.LatestResultSeq != d.Generation {
			return false, nil
		}
		event, e := s.readEvent(ctx, q, op, op.LatestResultSeq)
		if e != nil {
			return false, e
		}
		defer clear(event.Body)
		p, _ := parseEvent(event.Body)
		return observationRepairable(p), nil
	}
	var state string
	if e = q.QueryRowContext(ctx, `SELECT delivery_state FROM outbox WHERE operation_id=? AND kind=? AND generation=?`, d.OperationID, d.Kind, d.Generation).Scan(&state); e != nil {
		return false, ErrEvidence
	}
	if state != "pending" && !(historical && state == "confirmed") {
		return false, nil
	}
	switch d.Kind {
	case "prepare":
		if e = s.requireBinding(ctx, q, b, false); errors.Is(e, ErrFrozen) {
			return false, nil
		} else if e != nil {
			return false, e
		}
		return op.OwnerBoot == s.boot && op.PreparedSession == b.SessionID && op.State == "accepted" && !op.CancelRequested, nil
	case "cancel":
		return op.CancelRequested && op.LatestResultSeq == 0, nil
	case "finalize":
		_, _, e := s.finalizeContext(ctx, q, b)
		if errors.Is(e, ErrFrozen) || errors.Is(e, ErrRecoveryIncomplete) {
			return false, nil
		}
		return e == nil, e
	}
	return false, ErrEvidence
}

// Only fixed diagnostic codes are persisted; callers cannot save error text or
// payload secrets through this method. The already reserved retry is retained.
func (s *Store) RecordDeliveryFailure(ctx context.Context, b Binding, messageID, code string, now time.Time) error {
	if s.version != 5 {
		return ErrSchema
	}
	if !validID(messageID) {
		return ErrInput
	}
	switch code {
	case "write_failed", "connection_closed", "queue_full", "timeout", "cursor_expired", "observation_failed":
	default:
		return ErrInput
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if e = s.requireBinding(ctx, tx, b, true); e != nil {
		return e
	}
	round, e := s.deliveryRound(ctx, tx, b)
	if e != nil {
		return e
	}
	var attempts uint64
	var kind, previous string
	if e = tx.QueryRowContext(ctx, `SELECT attempts,kind,failure_code FROM runtime_deliveries WHERE message_id=? AND node_id=? AND ledger_id=? AND session_id=? AND manager_boot=? AND round_id=?`, messageID, b.NodeID, b.LedgerID, b.SessionID, s.boot, round).Scan(&attempts, &kind, &previous); errors.Is(e, sql.ErrNoRows) {
		return ErrNotFound
	} else if e != nil {
		return ErrStorage
	}
	if (code == "cursor_expired" || code == "observation_failed") && kind != "observe" {
		return ErrInput
	}
	if previous == "cursor_expired" || previous == "observation_failed" {
		if kind != "observe" {
			return ErrEvidence
		}
		// Stopping a query is a durable decision for this exact context.
		// A concurrent writer's later transport error cannot revive it.
		if e = tx.Commit(); e != nil {
			return ErrStorage
		}
		return nil
	}
	result, e := tx.ExecContext(ctx, `UPDATE runtime_deliveries SET next_attempt=max(next_attempt,?),failure_code=? WHERE message_id=? AND node_id=? AND ledger_id=? AND session_id=? AND manager_boot=? AND round_id=?`, now.Add(deliveryDelay(attempts)).UnixMilli(), code, messageID, b.NodeID, b.LedgerID, b.SessionID, s.boot, round)
	if e != nil {
		return ErrStorage
	}
	n, e := result.RowsAffected()
	if e != nil {
		return ErrStorage
	}
	if n != 1 {
		return ErrNotFound
	}
	if e = tx.Commit(); e != nil {
		return ErrStorage
	}
	return nil
}

// Completed scans no longer confer an active recovery context. This is the
// same normalization used by finalizeContext; a replacement round is distinct
// even if the authenticated transport session has not changed.
func (s *Store) deliveryRound(ctx context.Context, q querier, b Binding) (string, error) {
	if e := s.requireBinding(ctx, q, b, true); e != nil {
		return "", e
	}
	var round string
	if e := q.QueryRowContext(ctx, `SELECT active_round FROM recovery_nodes WHERE node_id=?`, b.NodeID).Scan(&round); e != nil {
		return "", ErrEvidence
	}
	if round != "" {
		scan, e := s.loadRecoveryScan(ctx, q, b)
		if e != nil {
			return "", e
		}
		if scan.Status == "completed" {
			return "", nil
		}
	}
	return round, nil
}

// A previously released result still needs the Agent's confirmation on the
// current all-scan round. The historical confirmed outbox cannot supply it.
func (s *Store) needsRecoveryFinalization(ctx context.Context, q querier, b Binding, op Operation) (bool, error) {
	round, e := s.deliveryRound(ctx, q, b)
	if e != nil {
		return false, e
	}
	if round == "" {
		return false, nil
	}
	scan, e := s.loadRecoveryScan(ctx, q, b)
	if e != nil {
		return false, e
	}
	if scan.Scope != "all" || scan.Status == "abandoned" || scan.Status == "expired" || scan.Status == "blocked" {
		return false, nil
	}
	var count int
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM recovery_members WHERE round_id=? AND operation_id=?`, round, op.OperationID).Scan(&count); e != nil {
		return false, ErrStorage
	}
	if count == 0 {
		return false, nil
	}
	var id string
	e = q.QueryRowContext(ctx, `SELECT message_id FROM finalize_requests WHERE operation_id=? AND node_id=? AND ledger_id=? AND session_id=? AND manager_boot=? AND agent_boot=? AND round_id=? AND accepted=1`, op.OperationID, b.NodeID, b.LedgerID, b.SessionID, s.boot, scan.AgentBootID, round).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return true, nil
	}
	if e != nil {
		return false, ErrStorage
	}
	request, e := s.readFinalizeRequest(ctx, q, id, op)
	if e != nil {
		return false, e
	}
	return !request.Accepted, nil
}
