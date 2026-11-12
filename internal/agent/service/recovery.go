package service

import (
	"docker-manager/internal/agent/ledger"
	"encoding/json"
	"errors"
	"time"
)

// Recover only authenticated, independently durable exits. This path never
// recreates a Gate, attaches a PID, or invokes Compose. Resource collection is
// left to the ordinary supplemental-observation query after the first result.
func (r *runtimeService) recoverStoredOutcome(id string) error {
	r.mu.Lock()
	o := r.ops[id]
	collecting := o != nil && o.collecting
	r.mu.Unlock()
	if collecting {
		return nil
	}
	fact, err := r.s.ReadProcessOutcome(r.ctx, id)
	if errors.Is(err, ledger.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer clear(fact.Output)
	var evidence ledger.ObservationEvidence
	if fact.Outcome == "normal_exit" {
		evidence.Observation, err = json.Marshal(map[string]any{
			"status": "error", "collected_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "items": []any{},
			"error": map[string]any{"category": "unavailable", "code": "resource_query_failed", "message": "Resource observation is unavailable.", "retry_action": "query_operation"},
		})
		if err != nil {
			return err
		}
	}
	if _, err = r.s.RecordProcessOutcome(r.ctx, id, fact, evidence); err == nil && o != nil {
		r.forgetCompleted(id, o)
	}
	return err
}

func (r *runtimeService) recoverPendingOutcomes() error {
	refs, err := r.s.PendingProcessOutcomeRefs(r.ctx, 32)
	if err != nil {
		return err
	}
	for _, id := range refs {
		if err := r.recoverStoredOutcome(id); err != nil {
			return err
		}
	}
	return nil
}

// Independent of the connection and long Compose work: a temporary result
// write failure must also converge while disconnected in the same boot.
func (r *runtimeService) runOutcomeRecovery() error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return nil
		case <-tick.C:
			if err := r.recoverPendingOutcomes(); err != nil && r.ctx.Err() == nil && !errors.Is(err, ledger.ErrStorage) {
				return err
			}
		}
	}
}

func (r *runtimeService) recoveryFailure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recoveryError
}
