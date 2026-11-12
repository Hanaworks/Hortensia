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

type serviceStack struct {
	id, node, project, display, current string
	revision                            uint64
	applied, operation, observation     sql.NullString
	created                             int64
}

func readServiceStack(ctx context.Context, q querier, id string) (serviceStack, error) {
	var st serviceStack
	e := q.QueryRowContext(ctx, `SELECT id,node_id,project_name,display_name,current_version_id,revision,last_applied_version_id,last_operation_id,observation_operation_id,created_at FROM stacks WHERE id=?`, id).Scan(&st.id, &st.node, &st.project, &st.display, &st.current, &st.revision, &st.applied, &st.operation, &st.observation, &st.created)
	if errors.Is(e, sql.ErrNoRows) {
		return st, serviceError(404, "not_found")
	}
	if e != nil {
		return st, serviceStorageError(e)
	}
	return st, nil
}
func nullString(x sql.NullString) any {
	if x.Valid {
		return x.String
	}
	return nil
}
func (s *Store) stackView(ctx context.Context, q querier, st serviceStack) (map[string]any, error) {
	var observation any
	if st.observation.Valid {
		op, e := s.readOperation(ctx, q, st.observation.String)
		if e != nil {
			return nil, e
		}
		if op.StackID != st.id || op.NodeID != st.node || op.LatestResultSeq == 0 {
			return nil, ErrEvidence
		}
		if e = s.requireOperationEvidence(ctx, q, op); e != nil {
			return nil, e
		}
		event, e := s.readEvent(ctx, q, op, op.LatestResultSeq)
		if e != nil {
			return nil, e
		}
		defer clear(event.Body)
		var body map[string]json.RawMessage
		if json.Unmarshal(event.Body, &body) != nil {
			return nil, ErrEvidence
		}
		observation = body["observation"]
	}
	return map[string]any{"stack_id": st.id, "node_id": st.node, "project_name": st.project, "display_name": st.display, "current_version_id": st.current, "revision": st.revision, "last_applied_version_id": nullString(st.applied), "last_operation_id": nullString(st.operation), "observation": observation}, nil
}
func (s *Store) readConfig(ctx context.Context, q querier, st serviceStack, version string) ([]byte, error) {
	var sealed sealedBlob
	e := q.QueryRowContext(ctx, `SELECT package_cipher,wrapped_dek,key_id,fingerprint FROM config_versions WHERE id=? AND stack_id=?`, version, st.id).Scan(&sealed.Cipher, &sealed.WrappedDEK, &sealed.KeyID, &sealed.Fingerprint)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, serviceError(404, "not_found")
	}
	if e != nil {
		return nil, serviceStorageError(e)
	}
	raw, e := s.unseal(blobBinding{OperationID: version, NodeID: st.node, Digest: st.id, Kind: "config"}, sealed)
	if e != nil {
		return nil, e
	}
	if e = s.validator.ValidateConfig(raw); e != nil {
		clear(raw)
		return nil, ErrEvidence
	}
	return raw, nil
}
func (s *Store) mutateConfig(ctx context.Context, tx *sql.Tx, u serviceUser, r ServiceRequest, rt serviceRoute, b serviceBody, p *preparedService) (ServiceReply, string, string, error) {
	fail := func(e error) (ServiceReply, string, string, error) { return ServiceReply{}, "", "", e }
	node := rt.node
	var st serviceStack
	var e error
	if rt.kind == "versions" {
		st, e = readServiceStack(ctx, tx, rt.stack)
		if e != nil {
			return fail(e)
		}
		node = st.node
	}
	if e = requireCaps(ctx, tx, u, node, "node.read", "config.read", "config.write"); e != nil {
		return fail(e)
	}
	if rt.kind == "stacks" {
		var n int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM stacks WHERE node_id=? AND project_name=?`, node, b.ProjectName).Scan(&n); e != nil {
			return fail(serviceStorageError(e))
		}
		if n != 0 {
			return fail(serviceError(409, "name_conflict"))
		}
	} else {
		if e = checkETag(r.IfMatch, st.current); e != nil {
			return fail(e)
		}
	}
	if !p.ready {
		p.node = node
		p.stack = st.id
		return fail(errPrepareService)
	}
	if p.node != node || rt.kind == "versions" && p.stack != st.id {
		return fail(ErrEvidence)
	}
	var parent any
	now := time.Now().UnixMilli()
	if rt.kind == "stacks" {
		if _, e = tx.ExecContext(ctx, `INSERT INTO stacks(id,node_id,project_name,display_name,current_version_id,revision,created_at) VALUES(?,?,?,?,?,1,?)`, p.stack, node, b.ProjectName, b.ProjectName, p.version, now); e != nil {
			return fail(serviceStorageError(e))
		}
	} else {
		parent = st.current
		if _, e = tx.ExecContext(ctx, `UPDATE stacks SET current_version_id=?,revision=revision+1 WHERE id=?`, p.version, st.id); e != nil {
			return fail(serviceStorageError(e))
		}
	}
	sealed := p.sealed
	if sealed == nil {
		return fail(ErrEvidence)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO config_versions(id,stack_id,parent_id,created_by,created_at,package_cipher,wrapped_dek,key_id,fingerprint) VALUES(?,?,?,?,?,?,?,?,?)`, p.version, p.stack, parent, u.id, now, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint); e != nil {
		return fail(serviceStorageError(e))
	}
	body := map[string]any{"version_id": p.version}
	typ, id := "version", p.version
	if rt.kind == "stacks" {
		body["stack_id"] = p.stack
		typ, id = "stack", p.stack
	}
	reply := serviceReply(201, body)
	reply.ETag = `"` + p.version + `"`
	return reply, typ, id, nil
}
func (s *Store) mutateApply(ctx context.Context, tx *sql.Tx, u serviceUser, r ServiceRequest, rt serviceRoute, b serviceBody, p *preparedService) (ServiceReply, string, string, error) {
	fail := func(e error) (ServiceReply, string, string, error) { return ServiceReply{}, "", "", e }
	st, e := readServiceStack(ctx, tx, rt.stack)
	if e != nil {
		return fail(e)
	}
	if e = requireCaps(ctx, tx, u, st.node, "node.read", "config.read", "stack.deploy"); e != nil {
		return fail(e)
	}
	config, e := s.readConfig(ctx, tx, st, b.VersionID)
	if e != nil {
		return fail(e)
	}
	defer clear(config)
	var ledger, session, boot, caps string
	var frozen bool
	var profile sql.NullString
	var revoked sql.NullInt64
	e = tx.QueryRowContext(ctx, `SELECT ledger_id,session_id,boot_id,frozen,execution_profile_json,agent_capabilities_json,revoked_at FROM nodes WHERE id=?`, st.node).Scan(&ledger, &session, &boot, &frozen, &profile, &caps, &revoked)
	if e != nil {
		return fail(serviceStorageError(e))
	}
	if session == "" || boot != s.boot || revoked.Valid {
		return fail(serviceError(503, "node_unavailable"))
	}
	if frozen {
		return fail(serviceError(409, "recovery_required"))
	}
	binding := Binding{st.node, ledger, session}
	if e = s.requireBinding(ctx, tx, binding, false); e != nil {
		return fail(e)
	}
	if !profile.Valid {
		return fail(serviceError(422, "unsupported"))
	}
	var capabilities []string
	json.Unmarshal([]byte(caps), &capabilities)
	required := []string{"compose.apply.v1", "config.bundle.v1"}
	for _, c := range required {
		found := false
		for _, got := range capabilities {
			found = found || got == c
		}
		if !found {
			return fail(serviceError(422, "unsupported"))
		}
	}
	payload, e := json.Marshal(map[string]any{"operation_type": "stack.apply", "node_id": st.node, "stack_id": st.id, "version_id": b.VersionID, "project_name": st.project, "config": json.RawMessage(config), "required_capabilities": required, "execution_profile": json.RawMessage(profile.String)})
	if e != nil {
		return fail(ErrEvidence)
	}
	digest, e := s.InputDigest(payload)
	if e != nil {
		return fail(e)
	}
	meta, revision, e := payloadMetadata(payload)
	if e != nil {
		return fail(e)
	}
	if !p.ready {
		p.payload = payload
		p.node = st.node
		p.binding = binding
		p.operation = Operation{UserID: u.id, NodeID: st.node, LedgerID: ledger, StackID: st.id, VersionID: b.VersionID, ProjectName: st.project, EndpointID: meta.Profile.Endpoint, PolicyRevision: revision, Digest: digest, OwnerBoot: s.boot, PreparedSession: session, State: "accepted", Revision: 1, ReleaseStatus: "held"}
		return fail(errPrepareService)
	}
	if p.binding != binding || !bytes.Equal(p.payload, payload) {
		return fail(serviceError(409, "recovery_required"))
	}
	candidate := p.operation
	candidate.OperationID = p.id
	encoded, e := protocol.NewMessage("prepare", session, "", prepareFields{Operation: p.id, Digest: digest, Node: st.node, Stack: st.id, Version: b.VersionID, Payload: base64.StdEncoding.EncodeToString(payload)})
	if e != nil {
		return fail(serviceStorageError(e))
	}
	if _, e = s.validator.Validate(encoded); e != nil {
		return fail(serviceError(422, "unsupported"))
	}
	if _, _, e = s.acceptTx(ctx, tx, binding, candidate, payload, p.sealed, r.IdempotencyKey); e != nil {
		if errors.Is(e, ErrClaimed) {
			e = serviceError(409, "resource_busy")
		}
		return fail(e)
	}
	if _, e = tx.ExecContext(ctx, `UPDATE stacks SET last_operation_id=? WHERE id=?`, p.id, st.id); e != nil {
		return fail(serviceStorageError(e))
	}
	reply := serviceReply(202, map[string]any{"operation_id": p.id, "state": "accepted"})
	reply.Location = "/api/v1/operations/" + p.id
	return reply, "operation", p.id, nil
}
