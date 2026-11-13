package ledger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"path"
	"strings"
)

func inspectionDocumentShape(kind string, raw []byte) error {
	var v any
	switch kind {
	case "input":
		v = new([]byte)
	case "preparation":
		v = new(PreparationEvidence)
	case "process":
		v = new(ProcessOutcome)
	case "candidate":
		v = new(ownershipCandidate)
	default:
		return ErrEvidence
	}
	if json.Unmarshal(raw, v) != nil {
		return ErrEvidence
	}
	encoded, err := json.Marshal(v)
	defer clear(encoded)
	if err != nil || !bytes.Equal(raw, encoded) {
		return ErrEvidence
	}
	return nil
}

func (s *Store) inspectRuntimeSemantics(ctx context.Context, tx *sql.Tx, control scanControl) error {
	if !validID(control.boot) || (control.session != "" && !validID(control.session)) || (control.active != "" && !validID(control.active)) || (control.completed != "" && !validID(control.completed)) || (control.active != "" && control.active == control.completed) {
		return ErrEvidence
	}
	if !control.frozen && (control.session == "" || control.completed == "") {
		return ErrEvidence
	}
	var quarantine bool
	if tx.QueryRowContext(ctx, `SELECT quarantined FROM release_control WHERE id=1`).Scan(&quarantine) != nil {
		return ErrStorage
	}
	if quarantine && (!control.frozen || !control.dirty) {
		return ErrEvidence
	}
	p, err := readRuntimeProfile(ctx, tx)
	if err != nil {
		return err
	}
	if !validID(p.EndpointID) || p.PolicyRevision > 9007199254740991 {
		return ErrEvidence
	}
	if p.EngineID == "" {
		if err = inspectionExpect(ctx, tx, 0, `SELECT count(*) FROM executions`); err != nil {
			return err
		}
	}
	after := ""
	for {
		ids, e := scanOperationIDs(ctx, tx, after, 64)
		if e != nil {
			return e
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = s.inspectOperation(ctx, tx, id, p, quarantine); err != nil {
				return err
			}
		}
		after = ids[len(ids)-1]
	}
	if err = s.inspectBindings(ctx, tx); err != nil {
		return err
	}
	if err = s.inspectScans(ctx, tx, control); err != nil {
		return err
	}
	return s.inspectQueries(ctx, tx)
}

func (s *Store) inspectOperation(ctx context.Context, tx *sql.Tx, id string, p RuntimeProfile, quarantine bool) error {
	r, err := s.readRecord(ctx, tx, id)
	if err != nil {
		return err
	}
	if !validID(id) || !validID(r.StackID) || !validID(r.VersionID) || !validID(r.PreparedSession) || !validID(r.BootID) || r.EndpointID != p.EndpointID || r.PolicyRevision > p.PolicyRevision || !validDigest(r.Digest) || !validDigest(r.LocalFingerprint) {
		return ErrEvidence
	}
	if r.Permit != nil {
		v := r.Permit
		if !validID(v.Session) || v.Session != r.PreparedSession || !validID(v.Nonce) || !validID(v.PermitID) || !validID(v.ReplyTo) || v.AuthRev > 9007199254740991 {
			return ErrEvidence
		}
	}
	state, err := readEventState(ctx, tx, id)
	if err != nil {
		return err
	}
	if state.Blocked && !quarantine {
		return ErrEvidence
	}
	var payload []byte
	if err = s.readDocument(ctx, tx, r, "input", 0, &payload); err != nil {
		return inspectionEvidence(err)
	}
	defer clear(payload)
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), mustInspectionDigest(r.LocalFingerprint)) {
		return ErrEvidence
	}
	probe, _ := json.Marshal(map[string]any{"protocol": 1, "type": "prepare", "message_id": s.identity.NodeID, "session_id": r.PreparedSession, "reply_to": nil, "body": map[string]any{"operation_id": id, "input_digest": r.Digest, "node_id": s.identity.NodeID, "stack_id": r.StackID, "version_id": r.VersionID, "payload_b64": base64.StdEncoding.EncodeToString(payload)}})
	_, err = s.validator.Validate(probe)
	clear(probe)
	if err != nil {
		return ErrEvidence
	}
	var input struct {
		Project string `json:"project_name"`
		Profile struct {
			Endpoint string      `json:"docker_endpoint_id"`
			Revision json.Number `json:"policy_revision"`
		} `json:"execution_profile"`
	}
	if json.Unmarshal(payload, &input) != nil {
		return ErrEvidence
	}
	rev, e := input.Profile.Revision.Float64()
	if e != nil || input.Project != r.ProjectName || input.Profile.Endpoint != r.EndpointID || uint64(rev) != r.PolicyRevision {
		return ErrEvidence
	}
	finalCount, err := inspectionCount(ctx, tx, `SELECT count(*) FROM finalizations WHERE operation_id=?`, id)
	if err != nil {
		return err
	}
	wantClaims := uint64(2)
	if finalCount != 0 {
		wantClaims = 0
	}
	if err = inspectionExpect(ctx, tx, wantClaims, `SELECT count(*) FROM local_claims WHERE operation_id=?`, id); err != nil {
		return err
	}
	if finalCount == 0 {
		if err = requireEventClaims(ctx, tx, r); err != nil {
			return err
		}
	}
	var prep PreparationEvidence
	prepErr := s.readDocument(ctx, tx, r, "preparation", 0, &prep)
	hasPrep := prepErr == nil
	if prepErr != nil && prepErr != ErrNotFound {
		return prepErr
	}
	if (r.Phase == PhasePreparing) == hasPrep {
		return ErrEvidence
	}
	if hasPrep {
		if err = s.inspectPreparation(ctx, tx, r, prep, p); err != nil {
			return err
		}
	}
	facts, err := inspectionCount(ctx, tx, `SELECT count(*) FROM process_facts WHERE operation_id=?`, id)
	if err != nil {
		return err
	}
	processDocs, err := inspectionCount(ctx, tx, `SELECT count(*) FROM runtime_documents WHERE operation_id=? AND kind='process'`, id)
	if err != nil {
		return err
	}
	if r.Phase == PhaseStartCommitted {
		if facts != 1 {
			return ErrEvidence
		}
		var boot, spawn, cancel string
		var pid sql.NullInt64
		if tx.QueryRowContext(ctx, `SELECT owner_boot,spawn_state,process_id,cancel_reason FROM process_facts WHERE operation_id=?`, id).Scan(&boot, &spawn, &pid, &cancel) != nil {
			return ErrStorage
		}
		if boot != r.BootID || (cancel != "" && cancel != "canceled") {
			return ErrEvidence
		}
		switch spawn {
		case "not_attempted", "not_started":
			if pid.Valid || processDocs != 0 {
				return ErrEvidence
			}
		case "created":
			if !pid.Valid || pid.Int64 <= 0 || processDocs != 0 {
				return ErrEvidence
			}
		case "exited":
			if !pid.Valid || pid.Int64 <= 0 || processDocs != 1 {
				return ErrEvidence
			}
			f, e := s.readProcessOutcome(ctx, tx, r)
			if e != nil {
				return e
			}
			defer clear(f.Output)
			if cancel != "" && f.Outcome == "normal_exit" {
				return ErrEvidence
			}
		default:
			return ErrEvidence
		}
		if !state.LatestResultSeq.Valid && spawn == "not_started" {
			return ErrEvidence
		}
	} else if facts != 0 || processDocs != 0 {
		return ErrEvidence
	}
	if _, err = s.readRecoveryRecord(ctx, tx, id); err != nil {
		return inspectionEvidence(err)
	}
	if err = inspectionExpect(ctx, tx, 1, `SELECT count(*) FROM runtime_documents WHERE operation_id=? AND kind='input' AND seq=0`, id); err != nil {
		return err
	}
	var resultCount uint64
	for seq := uint64(1); seq <= state.LastSeq; seq++ {
		event, e := s.readEvent(ctx, tx, id, seq)
		if e != nil {
			return e
		}
		if event.Kind == "result" {
			resultCount++
			if e = s.inspectResult(ctx, tx, r, event, prep); e != nil {
				clear(event.Body)
				return e
			}
		}
		clear(event.Body)
	}
	if err = inspectionExpect(ctx, tx, resultCount, `SELECT count(*) FROM execution_events WHERE operation_id=? AND kind='result'`, id); err != nil {
		return err
	}
	wantDocuments := uint64(1)
	if hasPrep {
		wantDocuments++
	}
	wantDocuments += processDocs
	var nonStart uint64
	if tx.QueryRowContext(ctx, `SELECT count(*) FROM runtime_documents WHERE operation_id=? AND kind='candidate'`, id).Scan(&nonStart) != nil {
		return ErrStorage
	}
	wantDocuments += nonStart
	if err = inspectionExpect(ctx, tx, wantDocuments, `SELECT count(*) FROM runtime_documents WHERE operation_id=?`, id); err != nil {
		return err
	}
	if err = inspectionExpect(ctx, tx, 0, `SELECT count(*) FROM runtime_documents WHERE operation_id=? AND ((kind<>'candidate' AND seq<>0) OR (kind='candidate' AND NOT EXISTS(SELECT 1 FROM execution_events e WHERE e.operation_id=runtime_documents.operation_id AND e.event_seq=runtime_documents.seq AND e.kind='result')))`, id); err != nil {
		return err
	}
	return s.inspectConflicts(ctx, tx, r, state, quarantine)
}

func mustInspectionDigest(value string) []byte {
	b, _ := base64.RawURLEncoding.DecodeString(value)
	return b
}

func (s *Store) inspectPreparation(ctx context.Context, tx *sql.Tx, r Record, e PreparationEvidence, p RuntimeProfile) error {
	if e.OperationID != r.OperationID || e.InputFingerprint != r.LocalFingerprint || !validDigest(e.ManifestFingerprint) || e.WorkspaceID != path.Join("workspaces", r.StackID, r.VersionID, "package") || strings.ContainsAny(e.WorkspaceID, "\\:\x00") || e.EngineID == "" || e.EngineID != p.EngineID {
		return ErrEvidence
	}
	if r.PolicyRevision == p.PolicyRevision && (e.ProfileFingerprint != p.Fingerprint || e.DockerVersion != p.DockerVersion || e.ComposeVersion != p.ComposeVersion) {
		return ErrEvidence
	}
	if validateBaseline(e.Pre, r.ProjectName) != nil {
		return ErrEvidence
	}
	seen := map[string]bool{}
	external := map[string]bool{}
	for _, x := range e.Expected {
		k := x.Kind + "\x00" + x.LogicalKey
		if !validResourceKind(x.Kind) || x.LogicalKey == "" || seen[k] {
			return ErrEvidence
		}
		seen[k] = true
		external[k] = x.External
	}
	for _, x := range e.Pre.Resources {
		if x.Classification == "external" && (x.Kind == "container" || !external[x.Kind+"\x00"+x.LogicalKey]) {
			return ErrEvidence
		}
	}
	var stack string
	if tx.QueryRowContext(ctx, `SELECT stack_id FROM project_bindings WHERE endpoint_id=? AND project_name=?`, r.EndpointID, r.ProjectName).Scan(&stack) != nil || stack != r.StackID {
		return ErrEvidence
	}
	if e.OwnershipRevision == 0 {
		if len(managedResources(e.Pre)) != 0 {
			return ErrEvidence
		}
	} else {
		pub, err := s.inspectPublicationAt(ctx, tx, r.EndpointID, r.ProjectName, e.OwnershipRevision)
		if err != nil {
			return err
		}
		if pub.record.StackID != r.StackID || !sameResources(pub.candidate.Post, e.Pre) {
			return ErrEvidence
		}
	}
	// Same profile revision is immutable across preparations, including history.
	rows, err := tx.QueryContext(ctx, `SELECT x.operation_id FROM executions x JOIN runtime_documents d ON d.operation_id=x.operation_id AND d.kind='preparation' AND d.seq=0 WHERE x.docker_endpoint_id=? AND x.policy_revision=? AND x.operation_id<? ORDER BY x.operation_id LIMIT 1`, r.EndpointID, r.PolicyRevision, r.OperationID)
	if err != nil {
		return ErrStorage
	}
	var before string
	if rows.Next() {
		if rows.Scan(&before) != nil {
			rows.Close()
			return ErrStorage
		}
	}
	er, cl := rows.Err(), rows.Close()
	if er != nil || cl != nil {
		return ErrStorage
	}
	if before != "" {
		prior, er := s.readRecord(ctx, tx, before)
		if er != nil {
			return er
		}
		var pe PreparationEvidence
		er = s.readDocument(ctx, tx, prior, "preparation", 0, &pe)
		if er != ErrNotFound && er != nil {
			return er
		}
		if er == nil && (pe.ProfileFingerprint != e.ProfileFingerprint || pe.EngineID != e.EngineID || pe.DockerVersion != e.DockerVersion || pe.ComposeVersion != e.ComposeVersion) {
			return ErrEvidence
		}
	}
	return nil
}

func (s *Store) inspectResult(ctx context.Context, tx *sql.Tx, r Record, event Event, prep PreparationEvidence) error {
	var result runtimeResult
	if json.Unmarshal(event.Body, &result) != nil {
		return ErrEvidence
	}
	n, err := inspectionCount(ctx, tx, `SELECT count(*) FROM runtime_documents WHERE operation_id=? AND kind='candidate' AND seq=?`, r.OperationID, event.Seq)
	if err != nil {
		return err
	}
	if result.Outcome == "not_started" {
		if n != 0 || result.Ownership != "not_checked" || result.Output != nil || result.Observation != nil && !bytes.Equal(result.Observation, []byte("null")) || result.Error == nil {
			return ErrEvidence
		}
		if *result.Error != *fixedError(result.Error.Code) {
			return ErrEvidence
		}
		return nil
	}
	if n != 1 {
		return ErrEvidence
	}
	var c ownershipCandidate
	if err = s.readDocument(ctx, tx, r, "candidate", event.Seq, &c); err != nil {
		return err
	}
	if c.PreRevision != prep.OwnershipRevision || c.Status != result.Ownership || (c.Status != "consistent" && c.Status != "unknown" && c.Status != "drifted") {
		return ErrEvidence
	}
	fact, err := s.readProcessOutcome(ctx, tx, r)
	if err != nil {
		return err
	}
	defer clear(fact.Output)
	var want *runtimeError
	if fact.Outcome == "normal_exit" && fact.ExitCode != nil && *fact.ExitCode != 0 {
		code := fact.ErrorCode
		if code == "" {
			code = "compose_exit_nonzero"
		}
		want = fixedError(code)
	} else if fact.Outcome != "normal_exit" {
		code := fact.ErrorCode
		if code == "" {
			code = fact.Outcome
		}
		want = fixedError(code)
	}
	if (want == nil) != (result.Error == nil) || (want != nil && *want != *result.Error) {
		return ErrEvidence
	}
	if c.Status == "consistent" {
		if result.Outcome != "normal_exit" {
			return ErrEvidence
		}
		if err = inspectConsistentOwnership(r, prep, c, result.Observation); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) inspectConflicts(ctx context.Context, tx *sql.Tx, r Record, state eventState, quarantine bool) error {
	if err := inspectionExpect(ctx, tx, 0, `SELECT count(*) FROM event_conflicts c JOIN execution_events e ON e.operation_id=c.operation_id AND e.event_seq=c.event_seq WHERE c.operation_id=? AND c.body_fingerprint=e.body_fingerprint`, r.OperationID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_seq,body_fingerprint FROM event_conflicts WHERE operation_id=? ORDER BY event_seq,body_fingerprint`, r.OperationID)
	if err != nil {
		return ErrStorage
	}
	for rows.Next() {
		var seq uint64
		var fingerprint []byte
		if rows.Scan(&seq, &fingerprint) != nil {
			rows.Close()
			return ErrStorage
		}
		if !quarantine || !state.Blocked || !state.LatestResultSeq.Valid || seq != uint64(state.LatestResultSeq.Int64) || len(fingerprint) != 32 {
			rows.Close()
			return ErrEvidence
		}
	}
	e, c := rows.Err(), rows.Close()
	if e != nil || c != nil {
		return ErrStorage
	}
	return nil
}
