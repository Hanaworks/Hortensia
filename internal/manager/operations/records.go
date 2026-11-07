package operations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"docker-manager/internal/protocol"
)

func (s *Store) readOperation(ctx context.Context, q querier, id string) (Operation, error) {
	var o Operation
	var digest []byte
	var created, updated int64
	err := q.QueryRowContext(ctx, `SELECT operation_id,user_id,node_id,ledger_id,stack_id,version_id,project_name,endpoint_id,policy_revision,input_digest,owner_boot,prepared_session,state,revision,cancel_requested,permit_id,permit_session,start_nonce,auth_rev,latest_result_seq,max_event_seq,release_status,blocked,created_at,updated_at FROM operations WHERE operation_id=?`, id).Scan(&o.OperationID, &o.UserID, &o.NodeID, &o.LedgerID, &o.StackID, &o.VersionID, &o.ProjectName, &o.EndpointID, &o.PolicyRevision, &digest, &o.OwnerBoot, &o.PreparedSession, &o.State, &o.Revision, &o.CancelRequested, &o.PermitID, &o.PermitSession, &o.StartNonce, &o.AuthRev, &o.LatestResultSeq, &o.MaxEventSeq, &o.ReleaseStatus, &o.Blocked, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, ErrStorage
	}
	for _, value := range []string{o.OperationID, o.UserID, o.NodeID, o.LedgerID, o.StackID, o.VersionID, o.EndpointID, o.OwnerBoot, o.PreparedSession} {
		if !validID(value) {
			return Operation{}, ErrEvidence
		}
	}
	if len(digest) != 32 || o.Revision == 0 || o.Revision > 9007199254740991 || o.PolicyRevision > 9007199254740991 || o.LatestResultSeq > o.MaxEventSeq || o.MaxEventSeq > 2 {
		return Operation{}, ErrEvidence
	}
	if o.PermitID != "" {
		if !validID(o.PermitID) || !validID(o.PermitSession) || !validID(o.StartNonce) || o.AuthRev == 0 || o.AuthRev > 9007199254740991 {
			return Operation{}, ErrEvidence
		}
	} else if o.PermitSession != "" || o.StartNonce != "" || o.AuthRev != 0 {
		return Operation{}, ErrEvidence
	}
	switch o.State {
	case "accepted", "ready", "authorized", "running", "succeeded", "failed", "canceled", "reconciliation_required":
	default:
		return Operation{}, ErrEvidence
	}
	if o.ReleaseStatus != "held" && o.ReleaseStatus != "pending" && !(s.version == 2 && o.ReleaseStatus == "released") {
		return Operation{}, ErrEvidence
	}
	if o.ReleaseStatus == "pending" && (!terminalState(o.State) || o.LatestResultSeq == 0 || o.Blocked) {
		return Operation{}, ErrEvidence
	}
	if (o.State == "authorized" || o.State == "running") && o.PermitID == "" {
		return Operation{}, ErrEvidence
	}
	if (o.State == "accepted" || o.State == "ready") && o.PermitID != "" {
		return Operation{}, ErrEvidence
	}
	if o.Blocked && o.State != "reconciliation_required" {
		return Operation{}, ErrEvidence
	}
	o.Digest = base64.RawURLEncoding.EncodeToString(digest)
	o.CreatedAt = timestamp(created)
	o.UpdatedAt = timestamp(updated)
	return o, nil
}

func readClaims(ctx context.Context, q querier, id string) ([]Claim, error) {
	rows, err := q.QueryContext(ctx, "SELECT resource_kind,resource_id FROM resource_claims WHERE operation_id=? ORDER BY resource_kind,resource_id", id)
	if err != nil {
		return nil, ErrStorage
	}
	defer rows.Close()
	claims := make([]Claim, 0, 2)
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.Kind, &c.ResourceID); err != nil {
			return nil, ErrStorage
		}
		claims = append(claims, c)
	}
	if rows.Err() != nil {
		return nil, ErrStorage
	}
	return claims, nil
}

func (s *Store) requireClaims(ctx context.Context, q querier, o Operation) error {
	if o.ReleaseStatus == "released" {
		return s.verifyAbsenceRelease(ctx, q, o)
	}
	claims, err := readClaims(ctx, q, o.OperationID)
	if err != nil {
		return err
	}
	if len(claims) != 2 || claims[0] != (Claim{"project", projectResourceID(o.EndpointID, o.ProjectName)}) || claims[1] != (Claim{"stack", o.StackID}) {
		return ErrEvidence
	}
	var sameNode int
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM resource_claims WHERE operation_id=? AND node_id=?", o.OperationID, o.NodeID).Scan(&sameNode); err != nil {
		return ErrStorage
	}
	if sameNode != 2 {
		return ErrEvidence
	}
	return nil
}

type prepareFields struct {
	Operation string `json:"operation_id"`
	Digest    string `json:"input_digest"`
	Node      string `json:"node_id"`
	Stack     string `json:"stack_id"`
	Version   string `json:"version_id"`
	Payload   string `json:"payload_b64"`
}

type payloadFields struct {
	Node    string `json:"node_id"`
	Stack   string `json:"stack_id"`
	Version string `json:"version_id"`
	Project string `json:"project_name"`
	Profile struct {
		Endpoint string      `json:"docker_endpoint_id"`
		Revision json.Number `json:"policy_revision"`
	} `json:"execution_profile"`
}

func payloadMetadata(raw []byte) (payloadFields, uint64, error) {
	var p payloadFields
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, 0, ErrInput
	}
	// The protocol validator has already required an exact safe integer. This
	// conversion accepts 1e0/1.0 without reencoding the saved payload.
	n, err := p.Profile.Revision.Float64()
	if err != nil || n < 0 || n > 9007199254740991 || float64(uint64(n)) != n {
		return p, 0, ErrInput
	}
	return p, uint64(n), nil
}

func (s *Store) readPayload(ctx context.Context, q querier, o Operation) ([]byte, error) {
	var sealed sealedBlob
	err := q.QueryRowContext(ctx, "SELECT payload_cipher,wrapped_dek,key_id,fingerprint FROM operations WHERE operation_id=?", o.OperationID).Scan(&sealed.Cipher, &sealed.WrappedDEK, &sealed.KeyID, &sealed.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEvidence
	}
	if err != nil {
		return nil, ErrStorage
	}
	raw, err := s.unseal(blobBinding{OperationID: o.OperationID, NodeID: o.NodeID, Digest: o.Digest, Kind: "payload", Seq: 0}, sealed)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	digest, err := s.InputDigest(raw)
	if err != nil {
		return nil, err
	}
	if digest != o.Digest {
		return nil, ErrEvidence
	}
	// A synthetic envelope is validation-only. Its base64 value preserves the
	// original payload bytes; it is never sent as a historical prepare.
	encoded, err := protocol.NewMessage("prepare", o.PreparedSession, "", prepareFields{Operation: o.OperationID, Digest: o.Digest, Node: o.NodeID, Stack: o.StackID, Version: o.VersionID, Payload: base64.StdEncoding.EncodeToString(raw)})
	if err != nil {
		return nil, ErrStorage
	}
	if _, err = s.validator.Validate(encoded); err != nil {
		return nil, ErrEvidence
	}
	p, revision, err := payloadMetadata(raw)
	if err != nil || p.Node != o.NodeID || p.Stack != o.StackID || p.Version != o.VersionID || p.Project != o.ProjectName || p.Profile.Endpoint != o.EndpointID || revision != o.PolicyRevision {
		return nil, ErrEvidence
	}
	return append([]byte(nil), raw...), nil
}

func (s *Store) requireOperationEvidence(ctx context.Context, q querier, op Operation) error {
	if err := s.requireClaims(ctx, q, op); err != nil {
		return err
	}
	payload, err := s.readPayload(ctx, q, op)
	if err != nil {
		return err
	}
	clear(payload)
	if _, err = s.validateControlIntents(ctx, q, op); err != nil {
		return err
	}
	return s.validateOperationEvents(ctx, q, op)
}

func (s *Store) Get(ctx context.Context, id string) (Operation, error) {
	if !validID(id) {
		return Operation{}, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, ErrStorage
	}
	defer tx.Rollback()
	o, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return Operation{}, err
	}
	if err = s.requireOperationEvidence(ctx, tx, o); err != nil {
		return Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Operation{}, ErrStorage
	}
	return o, nil
}

func (s *Store) ReadPayload(ctx context.Context, id string) ([]byte, error) {
	if !validID(id) {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	o, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	raw, err := s.readPayload(ctx, tx, o)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		clear(raw)
		return nil, ErrStorage
	}
	return raw, nil
}

func (s *Store) Claims(ctx context.Context, id string) ([]Claim, error) {
	if !validID(id) {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	o, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err = s.requireClaims(ctx, tx, o); err != nil {
		return nil, err
	}
	claims, err := readClaims(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	return claims, nil
}

func (s *Store) validateControlIntents(ctx context.Context, q querier, op Operation) ([]Intent, error) {
	rows, err := q.QueryContext(ctx, "SELECT kind,delivery_state,terminal_state,generation,result_event_seq FROM outbox WHERE operation_id=? ORDER BY kind,generation", op.OperationID)
	if err != nil {
		return nil, ErrStorage
	}
	intents := make([]Intent, 0, 3)
	for rows.Next() {
		var intent Intent
		if err = rows.Scan(&intent.Kind, &intent.State, &intent.TerminalState, &intent.Generation, &intent.ResultEventSeq); err != nil {
			rows.Close()
			return nil, ErrStorage
		}
		intents = append(intents, intent)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, ErrStorage
	}
	if err = rows.Close(); err != nil {
		return nil, ErrStorage
	}
	var prepare, cancel *Intent
	for i := range intents {
		intent := &intents[i]
		if intent.Generation != 1 {
			return nil, ErrEvidence
		}
		switch intent.Kind {
		case "prepare":
			if prepare != nil {
				return nil, ErrEvidence
			}
			prepare = intent
		case "cancel":
			if cancel != nil {
				return nil, ErrEvidence
			}
			cancel = intent
		case "finalize": // The authenticated event projection validates this row.
		default:
			return nil, ErrEvidence
		}
	}
	if prepare == nil || (cancel != nil) != op.CancelRequested {
		return nil, ErrEvidence
	}
	if op.ReleaseStatus == "released" {
		if err := s.verifyAbsenceRelease(ctx, q, op); err != nil {
			return nil, err
		}
		return intents, nil
	}
	wantPrepare := "confirmed"
	if op.State == "accepted" && op.MaxEventSeq == 0 {
		wantPrepare = "pending"
		if op.CancelRequested {
			wantPrepare = "suppressed"
		}
	}
	if op.Blocked {
		// Conflict isolation preserves the last delivery fact; it does not
		// manufacture an Agent receipt or rewrite a prior cancellation.
		if prepare.State != "pending" && prepare.State != "confirmed" && !(prepare.State == "suppressed" && op.CancelRequested) {
			return nil, ErrEvidence
		}
	} else if prepare.State != wantPrepare {
		return nil, ErrEvidence
	}
	if cancel != nil && (cancel.State != "pending" && cancel.State != "confirmed" || cancel.State == "confirmed" && op.LatestResultSeq == 0) {
		return nil, ErrEvidence
	}
	return intents, nil
}

func (s *Store) Intents(ctx context.Context, id string) ([]Intent, error) {
	if !validID(id) {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	op, err := s.readOperation(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	intents, err := s.validateControlIntents(ctx, tx, op)
	if err != nil {
		return nil, err
	}
	if err = s.validateOperationEvents(ctx, tx, op); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	return intents, nil
}

// AcceptFixture validates a trusted frozen-input fixture. It is deliberately
// separate from the future HTTP idempotency and configuration repositories.
func (s *Store) AcceptFixture(ctx context.Context, b Binding, user string, raw []byte) (Operation, error) {
	if !validID(user) {
		return Operation{}, ErrInput
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "prepare" || env.SessionID == nil {
		return Operation{}, ErrInput
	}
	if *env.SessionID != b.SessionID {
		return Operation{}, ErrFrozen
	}
	var body prepareFields
	if err = json.Unmarshal(env.Body, &body); err != nil {
		return Operation{}, ErrInput
	}
	if body.Node != b.NodeID {
		return Operation{}, ErrIdentity
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(body.Payload)
	if err != nil {
		return Operation{}, ErrInput
	}
	defer clear(payload)
	digest, err := s.InputDigest(payload)
	if err != nil {
		return Operation{}, err
	}
	if digest != body.Digest {
		return Operation{}, ErrConflict
	}
	p, revision, err := payloadMetadata(payload)
	if err != nil {
		return Operation{}, err
	}
	candidate := Operation{OperationID: body.Operation, UserID: user, NodeID: body.Node, LedgerID: b.LedgerID, StackID: body.Stack, VersionID: body.Version, ProjectName: p.Project, EndpointID: p.Profile.Endpoint, PolicyRevision: revision, Digest: digest, OwnerBoot: s.boot, PreparedSession: b.SessionID, State: "accepted", Revision: 1, ReleaseStatus: "held"}
	// The first transaction handles cheap replays without consuming another
	// wrapping allowance; the second rechecks after encryption under contention.
	op, exists, err := s.accept(ctx, b, candidate, payload, nil)
	if err != nil || exists {
		return op, err
	}
	sealed, err := s.seal(ctx, blobBinding{OperationID: candidate.OperationID, NodeID: candidate.NodeID, Digest: candidate.Digest, Kind: "payload", Seq: 0}, payload)
	if err != nil {
		return Operation{}, err
	}
	op, _, err = s.accept(ctx, b, candidate, payload, &sealed)
	return op, err
}

func (s *Store) accept(ctx context.Context, b Binding, candidate Operation, payload []byte, sealed *sealedBlob) (Operation, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, false, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireBinding(ctx, tx, b, false); err != nil {
		return Operation{}, false, err
	}
	if _, err = s.requirePermission(ctx, tx, candidate.UserID, candidate.NodeID); err != nil {
		return Operation{}, false, err
	}
	existing, err := s.readOperation(ctx, tx, candidate.OperationID)
	if err == nil {
		if existing.UserID != candidate.UserID || existing.NodeID != candidate.NodeID || existing.LedgerID != candidate.LedgerID || existing.StackID != candidate.StackID || existing.VersionID != candidate.VersionID || existing.ProjectName != candidate.ProjectName || existing.EndpointID != candidate.EndpointID || existing.PolicyRevision != candidate.PolicyRevision || existing.Digest != candidate.Digest {
			return Operation{}, false, ErrConflict
		}
		if err = s.requireClaims(ctx, tx, existing); err != nil {
			return Operation{}, false, err
		}
		old, err := s.readPayload(ctx, tx, existing)
		if err != nil {
			return Operation{}, false, err
		}
		equal := bytes.Equal(old, payload)
		clear(old)
		if !equal {
			return Operation{}, false, ErrConflict
		}
		if _, err = s.validateControlIntents(ctx, tx, existing); err != nil {
			return Operation{}, false, err
		}
		if err = s.validateOperationEvents(ctx, tx, existing); err != nil {
			return Operation{}, false, err
		}
		if err = tx.Commit(); err != nil {
			return Operation{}, false, ErrStorage
		}
		return existing, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Operation{}, false, err
	}
	claims := []Claim{{"stack", candidate.StackID}, {"project", projectResourceID(candidate.EndpointID, candidate.ProjectName)}}
	for _, claim := range claims {
		var owner string
		err = tx.QueryRowContext(ctx, "SELECT operation_id FROM resource_claims WHERE node_id=? AND resource_kind=? AND resource_id=?", candidate.NodeID, claim.Kind, claim.ResourceID).Scan(&owner)
		if err == nil {
			return Operation{}, false, ErrClaimed
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Operation{}, false, ErrStorage
		}
	}
	if sealed == nil {
		return Operation{}, false, nil
	}
	now := time.Now().UnixMilli()
	candidate.CreatedAt = timestamp(now)
	candidate.UpdatedAt = timestamp(now)
	digest, _ := base64.RawURLEncoding.Strict().DecodeString(candidate.Digest)
	_, err = tx.ExecContext(ctx, `INSERT INTO operations(operation_id,user_id,node_id,ledger_id,stack_id,version_id,project_name,endpoint_id,policy_revision,input_digest,owner_boot,prepared_session,payload_cipher,wrapped_dek,key_id,fingerprint,state,revision,cancel_requested,release_status,blocked,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,0,'held',0,?,?)`, candidate.OperationID, candidate.UserID, candidate.NodeID, candidate.LedgerID, candidate.StackID, candidate.VersionID, candidate.ProjectName, candidate.EndpointID, candidate.PolicyRevision, digest, candidate.OwnerBoot, candidate.PreparedSession, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint, candidate.State, now, now)
	if err != nil {
		return Operation{}, false, ErrStorage
	}
	for _, claim := range claims {
		if _, err = tx.ExecContext(ctx, "INSERT INTO resource_claims(node_id,resource_kind,resource_id,operation_id) VALUES(?,?,?,?)", candidate.NodeID, claim.Kind, claim.ResourceID, candidate.OperationID); err != nil {
			return Operation{}, false, ErrStorage
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO outbox(operation_id,kind,generation,delivery_state) VALUES(?,'prepare',1,'pending')", candidate.OperationID); err != nil {
		return Operation{}, false, ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return Operation{}, false, ErrStorage
	}
	return candidate, true, nil
}
