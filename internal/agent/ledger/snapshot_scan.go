package ledger

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
)

func (s *Store) inspectScans(ctx context.Context, tx *sql.Tx, c scanControl) error {
	if c.dirty && !c.frozen {
		return ErrEvidence
	}
	groups := map[string]uint64{}
	counts := map[string]uint64{}
	if c.active != "" {
		groups[c.active] = c.total
	}
	if c.completed != "" {
		groups[c.completed] = c.completedTotal
	}
	cols, err := inspectionColumns(ctx, tx, "recovery_members")
	if err != nil {
		return err
	}
	lastScan, lastOperation := "", ""
	err = inspectionWalk(ctx, tx, "recovery_members", cols, func(v []any) error {
		scan := v[0].(string)
		ordinal := uint64(v[1].(int64))
		operation := v[2].(string)
		last, result := uint64(v[3].(int64)), uint64(v[4].(int64))
		phase := v[5].(string)
		acked := v[6].(int64) == 1
		fingerprint := v[7].([]byte)
		if _, ok := groups[scan]; !ok || ordinal != counts[scan] {
			return ErrEvidence
		}
		if scan == lastScan && operation <= lastOperation {
			return ErrEvidence
		}
		lastScan, lastOperation = scan, operation
		counts[scan]++
		r, e := s.readRecord(ctx, tx, operation)
		if e != nil {
			return e
		}
		state, e := readEventState(ctx, tx, operation)
		if e != nil {
			return e
		}
		if last > state.LastSeq || (result != 0 && result != last) || (result == 0 && acked) {
			return ErrEvidence
		}
		var latest json.RawMessage
		var finalState, finalSeq any
		if result != 0 {
			event, e := s.readEvent(ctx, tx, operation, result)
			if e != nil {
				return e
			}
			defer clear(event.Body)
			if event.Kind != "result" || (acked && event.AckSession == "") {
				return ErrEvidence
			}
			latest = event.Body
		}
		switch phase {
		case PhasePreparing:
			if last != 0 || result != 0 {
				return ErrEvidence
			}
		case PhaseReady:
			if last != 0 || result != 0 || r.Phase == PhasePreparing {
				return ErrEvidence
			}
		case PhaseStartCommitted:
			if r.Phase != PhaseStartCommitted || last != 1 || result != 0 {
				return ErrEvidence
			}
		case "result":
			if result == 0 {
				return ErrEvidence
			}
		case "reconciliation_required":
		case "finalized":
			raw, f, e := s.readFinalization(ctx, tx, r, state)
			clear(raw)
			if e != nil || f.Seq != result || result != last {
				return ErrEvidence
			}
			finalState, finalSeq = f.Terminal, f.Seq
		default:
			return ErrEvidence
		}
		if result == 0 && last > 0 && (r.Phase != PhaseStartCommitted || last != 1) {
			return ErrEvidence
		}
		view := struct {
			Operation  string `json:"operation_id"`
			Digest     string `json:"input_digest"`
			Stack      string `json:"stack_id"`
			Version    string `json:"version_id"`
			Project    string `json:"project_name"`
			Phase      string `json:"phase"`
			Last       uint64 `json:"last_event_seq"`
			Result     any    `json:"result_event_seq"`
			Held       bool   `json:"claims_held"`
			Ack        bool   `json:"report_acked"`
			FinalState any    `json:"final_state"`
			FinalSeq   any    `json:"finalized_result_event_seq"`
		}{r.OperationID, r.Digest, r.StackID, r.VersionID, r.ProjectName, phase, last, nil, phase != "finalized", acked, finalState, finalSeq}
		if result != 0 {
			view.Result = result
		}
		raw, e := json.Marshal(view)
		if e != nil {
			return ErrEvidence
		}
		if latest == nil {
			latest = json.RawMessage("null")
		}
		raw = append(raw[:len(raw)-1], []byte(`,"latest_result":`)...)
		raw = append(raw, latest...)
		raw = append(raw, '}')
		defer clear(raw)
		want, e := s.scanFingerprint(raw)
		if e != nil {
			return e
		}
		if !hmac.Equal(want, fingerprint) {
			return ErrEvidence
		}
		probe, e := scanWire(s.identity.NodeID, s.identity.NodeID, scan, "all", s.identity.LedgerID, 1, []json.RawMessage{raw}, "", s.identity.NodeID)
		if e != nil {
			return e
		}
		defer clear(probe)
		if len(probe) > scanWireLimit {
			return ErrEvidence
		}
		if _, e = s.validator.Validate(probe); e != nil {
			return ErrEvidence
		}
		return nil
	})
	if err != nil {
		return err
	}
	for id, n := range groups {
		if counts[id] != n {
			return ErrEvidence
		}
	}
	if c.active != "" && c.scope == "all" {
		if err = inspectionExpect(ctx, tx, c.total, `SELECT count(*) FROM executions`); err != nil {
			return err
		}
	}
	if !c.frozen {
		return inspectionExpect(ctx, tx, 0, `SELECT count(*) FROM recovery_changes`)
	}
	return nil
}

func (s *Store) inspectQueries(ctx context.Context, tx *sql.Tx) error {
	cols, err := inspectionColumns(ctx, tx, "observation_queries")
	if err != nil {
		return err
	}
	err = inspectionWalk(ctx, tx, "observation_queries", cols, func(v []any) error {
		if !validID(v[0].(string)) || !validID(v[1].(string)) || !validID(v[2].(string)) {
			return ErrEvidence
		}
		if v[5] == nil {
			return nil
		}
		seq := uint64(v[5].(int64))
		if seq < 3 {
			return ErrEvidence
		}
		event, e := s.readEvent(ctx, tx, v[1].(string), seq)
		if e != nil {
			return e
		}
		defer clear(event.Body)
		var result runtimeResult
		if event.Kind != "result" || json.Unmarshal(event.Body, &result) != nil || result.Outcome != "normal_exit" {
			return ErrEvidence
		}
		return inspectionExpect(ctx, tx, 1, `SELECT count(*) FROM observation_queries WHERE operation_id=? AND result_seq=?`, v[1], seq)
	})
	if err != nil {
		return err
	}
	// Every later result is produced atomically with precisely one observation query.
	return inspectionExpect(ctx, tx, 0, `SELECT count(*) FROM execution_events e WHERE e.kind='result' AND e.event_seq>2 AND (SELECT count(*) FROM observation_queries q WHERE q.operation_id=e.operation_id AND q.result_seq=e.event_seq)<>1`)
}
