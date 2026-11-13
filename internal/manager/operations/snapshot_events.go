package operations

import (
	"database/sql"
	"errors"
)

// Inspection streams numeric sequence keys; normal public readers retain their
// current-session/writer contracts and are never used as inspection entrypoints.
func (i *snapshotInspector) operationEvidence(op Operation) error {
	s := i.view
	claims, err := i.count(`SELECT count(*) FROM resource_claims WHERE operation_id=?`, op.OperationID)
	if err != nil {
		return err
	}
	want := int64(2)
	if op.ReleaseStatus == "released" {
		want = 0
	}
	if claims != want {
		return ErrEvidence
	}
	if want == 2 {
		matched, err := i.count(`SELECT count(*) FROM resource_claims WHERE operation_id=? AND node_id=? AND ((resource_kind='stack' AND resource_id=?) OR (resource_kind='project' AND resource_id=?))`, op.OperationID, op.NodeID, op.StackID, projectResourceID(op.EndpointID, op.ProjectName))
		if err != nil {
			return err
		}
		if matched != 2 {
			return ErrEvidence
		}
	}
	if err := s.requireClaims(i.ctx, i.tx, op); err != nil {
		return err
	}
	payload, err := s.readPayload(i.ctx, i.tx, op)
	clear(payload)
	if err != nil {
		return err
	}
	if _, err = s.validateControlIntents(i.ctx, i.tx, op); err != nil {
		return err
	}
	if op.ReleaseStatus == "released" && op.LatestResultSeq == 0 {
		return s.verifyAbsenceRelease(i.ctx, i.tx, op)
	}
	total, err := i.count(`SELECT count(*) FROM operation_events WHERE operation_id=?`, op.OperationID)
	if err != nil {
		return err
	}
	var max, result uint64
	var previous []byte
	defer func() { clear(previous) }()
	state, release := "", "held"
	for {
		if err = i.ctx.Err(); err != nil {
			return err
		}
		var seq uint64
		err = i.tx.QueryRowContext(i.ctx, `SELECT event_seq FROM operation_events WHERE operation_id=? AND event_seq>? ORDER BY event_seq LIMIT 1`, op.OperationID, max).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return ErrStorage
		}
		event, err := s.readEvent(i.ctx, i.tx, op, seq)
		if err != nil {
			return err
		}
		max = seq
		if event.Kind == "result" {
			p, _ := parseEvent(event.Body)
			if p.seq == 2 && op.PermitID == "" {
				clear(event.Body)
				return ErrEvidence
			}
			if result > 0 {
				old, _ := parseEvent(previous)
				if result+1 != seq || !observationRepairable(old) || p.Outcome != "normal_exit" || !sameProcessFact(previous, event.Body) {
					clear(event.Body)
					return ErrEvidence
				}
			}
			if seq > 2 && result == 0 || seq == 1 && total != 1 {
				clear(event.Body)
				return ErrEvidence
			}
			state, release = resultProjection(p)
			result = seq
			clear(previous)
			previous = event.Body
		} else {
			clear(event.Body)
		}
	}
	if max != op.MaxEventSeq || result != op.LatestResultSeq {
		return ErrEvidence
	}
	if op.Blocked {
		if op.State != "reconciliation_required" || op.ReleaseStatus != "held" {
			return ErrEvidence
		}
	} else if result > 0 {
		if state != op.State || release != op.ReleaseStatus && !(release == "pending" && op.ReleaseStatus == "released") {
			return ErrEvidence
		}
	} else {
		if op.ReleaseStatus != "held" {
			return ErrEvidence
		}
		if max == 1 {
			if op.State != "running" {
				return ErrEvidence
			}
		} else if op.State != "accepted" && op.State != "ready" && op.State != "authorized" {
			return ErrEvidence
		}
	}
	var intentState, terminal string
	var intentSeq uint64
	err = i.tx.QueryRowContext(i.ctx, `SELECT delivery_state,result_event_seq,terminal_state FROM outbox WHERE operation_id=? AND kind='finalize' AND generation=1`, op.OperationID).Scan(&intentState, &intentSeq, &terminal)
	if release == "pending" && result > 0 {
		want := "pending"
		if op.ReleaseStatus == "released" {
			want = "confirmed"
		}
		if op.Blocked {
			want = "suppressed"
		}
		if err != nil || intentState != want || intentSeq != result || terminal != state {
			return ErrEvidence
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ErrEvidence
	}
	return nil
}
