package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path"
	"strings"
)

func (s *Store) ReceivePrepare(ctx context.Context, raw []byte) (PrepareReceipt, error) {
	if err := s.requireRuntime(); err != nil {
		return PrepareReceipt{}, err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "prepare" || env.SessionID == nil {
		return PrepareReceipt{}, ErrInput
	}
	var b struct {
		Operation string `json:"operation_id"`
		Digest    string `json:"input_digest"`
		Node      string `json:"node_id"`
		Stack     string `json:"stack_id"`
		Version   string `json:"version_id"`
		Payload   string `json:"payload_b64"`
	}
	if json.Unmarshal(env.Body, &b) != nil {
		return PrepareReceipt{}, ErrInput
	}
	if b.Node != s.identity.NodeID {
		return PrepareReceipt{}, ErrIdentity
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(b.Payload)
	if err != nil {
		return PrepareReceipt{}, ErrInput
	}
	defer clear(payload)
	var input struct {
		Project string `json:"project_name"`
		Profile struct {
			Endpoint string      `json:"docker_endpoint_id"`
			Revision json.Number `json:"policy_revision"`
		} `json:"execution_profile"`
	}
	if json.Unmarshal(payload, &input) != nil {
		return PrepareReceipt{}, ErrInput
	}
	revision, err := input.Profile.Revision.Float64()
	if err != nil {
		return PrepareReceipt{}, ErrInput
	}
	_, kek, key, err := s.eventKeyCopies()
	if err != nil {
		return PrepareReceipt{}, err
	}
	clear(kek)
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	fp := mac.Sum(nil)
	clear(key)
	r := Record{Identity: s.identity, OperationID: b.Operation, StackID: b.Stack, VersionID: b.Version, Digest: b.Digest, LocalFingerprint: base64.RawURLEncoding.EncodeToString(fp), ProjectName: input.Project, EndpointID: input.Profile.Endpoint, PolicyRevision: uint64(revision), PreparedSession: *env.SessionID, BootID: s.boot, Phase: PhasePreparing, CreatedAt: timestamp(s.runtime.now().UnixMilli())}
	// Replay/conflict is checked before reserving crypto quota, then rechecked in
	// the insertion transaction. No IO is authorized by either preliminary read.
	receipt, err := s.receivePrepare(ctx, r, payload, fp, nil)
	if err != nil || receipt.Disposition != "seal" {
		return receipt, err
	}
	sealed, err := s.sealDocument(ctx, r, "input", 0, payload)
	if err != nil {
		return PrepareReceipt{}, err
	}
	return s.receivePrepare(ctx, r, payload, fp, &sealed)
}
func (s *Store) receivePrepare(ctx context.Context, r Record, payload, fp []byte, sealed *sealedEvent) (PrepareReceipt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PrepareReceipt{}, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoverySession(ctx, tx, r.PreparedSession); err != nil {
		return PrepareReceipt{}, err
	}
	existing, err := s.readRecord(ctx, tx, r.OperationID)
	if err == nil {
		if existing.StackID != r.StackID || existing.VersionID != r.VersionID || existing.Digest != r.Digest || existing.LocalFingerprint != r.LocalFingerprint || existing.ProjectName != r.ProjectName || existing.EndpointID != r.EndpointID || existing.PolicyRevision != r.PolicyRevision {
			if err = s.quarantineRelease(ctx, tx); err != nil {
				return PrepareReceipt{}, err
			}
			if tx.Commit() != nil {
				return PrepareReceipt{}, ErrStorage
			}
			return PrepareReceipt{}, ErrConflict
		}
		var original []byte
		if err = s.readDocument(ctx, tx, existing, "input", 0, &original); err != nil {
			return PrepareReceipt{}, err
		}
		defer clear(original)
		if !hmac.Equal(original, payload) {
			return PrepareReceipt{}, ErrEvidence
		}
		state, err := readEventState(ctx, tx, r.OperationID)
		if err != nil {
			return PrepareReceipt{}, err
		}
		disposition := existing.Phase
		if state.LatestResultSeq.Valid {
			disposition = "reported"
		}
		if _, _, err = s.readFinalization(ctx, tx, existing, state); err == nil {
			disposition = "finalized"
		} else if err != ErrNotFound {
			return PrepareReceipt{}, err
		}
		return PrepareReceipt{existing, disposition}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return PrepareReceipt{}, err
	}
	if err = s.requireRecoveryMutation(ctx, tx, r.PreparedSession); err != nil {
		return PrepareReceipt{}, err
	}
	profile, err := readRuntimeProfile(ctx, tx)
	if err != nil {
		return PrepareReceipt{}, err
	}
	if profile.EngineID == "" || profile.EndpointID != r.EndpointID || profile.PolicyRevision != r.PolicyRevision {
		return PrepareReceipt{}, ErrConflict
	}
	if sealed == nil {
		return PrepareReceipt{Disposition: "seal"}, nil
	}
	digest, _ := base64.RawURLEncoding.DecodeString(r.Digest)
	if _, err = tx.ExecContext(ctx, `INSERT INTO executions(operation_id,stack_id,version_id,input_digest,local_fingerprint,project_name,docker_endpoint_id,policy_revision,prepared_session,owner_boot_id,phase,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.OperationID, r.StackID, r.VersionID, digest, fp, r.ProjectName, r.EndpointID, r.PolicyRevision, r.PreparedSession, s.boot, PhasePreparing, s.runtime.now().UnixMilli()); err != nil {
		return PrepareReceipt{}, ErrStorage
	}
	for _, c := range []Claim{{"project", projectResourceID(r.EndpointID, r.ProjectName)}, {"stack", r.StackID}} {
		var owner string
		if err = tx.QueryRowContext(ctx, `SELECT operation_id FROM local_claims WHERE resource_kind=? AND resource_id=?`, c.Kind, c.ResourceID).Scan(&owner); err == nil {
			return PrepareReceipt{}, ErrClaimed
		} else if err != sql.ErrNoRows {
			return PrepareReceipt{}, ErrStorage
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO local_claims(resource_kind,resource_id,operation_id) VALUES(?,?,?)`, c.Kind, c.ResourceID, r.OperationID); err != nil {
			return PrepareReceipt{}, ErrStorage
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO event_state(operation_id,last_seq,blocked) VALUES(?,0,0)`, r.OperationID); err != nil {
		return PrepareReceipt{}, ErrStorage
	}
	if err = s.insertDocument(ctx, tx, r, "input", 0, *sealed); err != nil {
		return PrepareReceipt{}, err
	}
	if tx.Commit() != nil {
		return PrepareReceipt{}, ErrStorage
	}
	return PrepareReceipt{r, "new"}, nil
}
func (s *Store) ReadExecutionInput(ctx context.Context, operationID string) ([]byte, error) {
	if err := s.requireRuntime(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	r, err := s.readRecord(ctx, tx, operationID)
	if err != nil {
		return nil, err
	}
	var raw []byte
	if err = s.readDocument(ctx, tx, r, "input", 0, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
func (s *Store) validatePrepared(ctx context.Context, q querier, r Record, e PreparationEvidence) error {
	if e.OperationID != r.OperationID || e.InputFingerprint != r.LocalFingerprint || !validDigest(e.ManifestFingerprint) || e.WorkspaceID != path.Join("workspaces", r.StackID, r.VersionID, "package") || strings.ContainsAny(e.WorkspaceID, "\\:\x00") || strings.HasPrefix(e.WorkspaceID, "/") || path.Clean(e.WorkspaceID) != e.WorkspaceID || strings.HasPrefix(e.WorkspaceID, "../") {
		return ErrEvidence
	}
	p, err := readRuntimeProfile(ctx, q)
	if err != nil {
		return err
	}
	if p.EngineID == "" || p.EndpointID != r.EndpointID || p.PolicyRevision != r.PolicyRevision || e.ProfileFingerprint != p.Fingerprint || e.EngineID != p.EngineID || e.DockerVersion != p.DockerVersion || e.ComposeVersion != p.ComposeVersion {
		return ErrConflict
	}
	if err = validateBaseline(e.Pre, r.ProjectName); err != nil {
		return err
	}
	keys := map[string]bool{}
	external := map[string]bool{}
	for _, x := range e.Expected {
		key := x.Kind + "\x00" + x.LogicalKey
		if !validResourceKind(x.Kind) || x.LogicalKey == "" || keys[key] {
			return ErrEvidence
		}
		keys[key] = true
		if x.External {
			external[key] = true
		}
	}
	for _, x := range e.Pre.Resources {
		if x.Classification == "external" && (!external[x.Kind+"\x00"+x.LogicalKey] || x.Kind == "container") {
			return ErrEvidence
		}
	}
	binding, err := s.readProjectBinding(ctx, q, r.EndpointID, r.ProjectName)
	if err == ErrNotFound {
		if e.OwnershipRevision != 0 || len(managedResources(e.Pre)) != 0 {
			return ErrConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	if binding.StackID != r.StackID || binding.Revision != e.OwnershipRevision || binding.Status == "drifted" || !sameResources(binding.Baseline, e.Pre) {
		return ErrConflict
	}
	return nil
}
func (s *Store) CommitPrepared(ctx context.Context, operationID string, e PreparationEvidence) (Record, error) {
	if err := s.requireRuntime(); err != nil {
		return Record{}, err
	}
	r, err := s.Get(ctx, operationID)
	if err != nil {
		return Record{}, err
	}
	if r.Phase == PhaseReady {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return Record{}, ErrStorage
		}
		defer tx.Rollback()
		if err = s.requireRecoveryMutation(ctx, tx, r.PreparedSession); err != nil {
			return Record{}, err
		}
		state, err := readEventState(ctx, tx, r.OperationID)
		if err != nil {
			return Record{}, err
		}
		if r.BootID != s.boot || state.LastSeq != 0 || state.Blocked {
			return Record{}, ErrConflict
		}
		var old PreparationEvidence
		if err = s.readDocument(ctx, tx, r, "preparation", 0, &old); err != nil {
			return Record{}, err
		}
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(e)
		if !hmac.Equal(a, b) {
			return Record{}, ErrConflict
		}
		return r, nil
	}
	sealed, err := s.sealDocument(ctx, r, "preparation", 0, e)
	if err != nil {
		return Record{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, ErrStorage
	}
	defer tx.Rollback()
	r, err = s.readRecord(ctx, tx, operationID)
	if err != nil {
		return Record{}, err
	}
	if err = s.requireRecoveryMutation(ctx, tx, r.PreparedSession); err != nil {
		return Record{}, err
	}
	if r.BootID != s.boot {
		return Record{}, ErrFrozen
	}
	state, err := readEventState(ctx, tx, operationID)
	if err != nil {
		return Record{}, err
	}
	if state.LastSeq != 0 || state.Blocked {
		return Record{}, ErrConflict
	}
	if r.Phase == PhaseReady {
		var existing PreparationEvidence
		if err = s.readDocument(ctx, tx, r, "preparation", 0, &existing); err != nil {
			return Record{}, err
		}
		a, _ := json.Marshal(existing)
		b, _ := json.Marshal(e)
		if !hmac.Equal(a, b) {
			return Record{}, ErrConflict
		}
		return r, nil
	}
	if r.Phase != PhasePreparing {
		return Record{}, ErrConflict
	}
	if err = requireEventClaims(ctx, tx, r); err != nil {
		return Record{}, err
	}
	if err = s.validatePrepared(ctx, tx, r, e); err != nil {
		return Record{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_bindings(endpoint_id,project_name,stack_id,ownership_revision,status) VALUES(?,?,?,0,'provisional') ON CONFLICT DO NOTHING`, r.EndpointID, r.ProjectName, r.StackID); err != nil {
		return Record{}, ErrStorage
	}
	if err = s.insertDocument(ctx, tx, r, "preparation", 0, sealed); err != nil {
		return Record{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE executions SET phase='ready' WHERE operation_id=?`, operationID); err != nil {
		return Record{}, ErrStorage
	}
	if tx.Commit() != nil {
		return Record{}, ErrStorage
	}
	r.Phase = PhaseReady
	return r, nil
}
