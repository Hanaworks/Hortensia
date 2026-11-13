package operations

import (
	"bytes"
	"crypto/hmac"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"docker-manager/internal/protocol"
)

func (i *snapshotInspector) row(t inspectionTable, r inspectionRow) ([]byte, error) {
	if err := i.clearRow(t.name, r); err != nil {
		return nil, err
	}
	s, q, ctx := i.view, i.tx, i.ctx
	switch t.name {
	case "operations":
		op, err := s.readOperation(ctx, q, r.s("operation_id"))
		if err != nil {
			return nil, err
		}
		if r.s("digest_key_id") != s.identity.DigestKeyID {
			return nil, ErrEvidence
		}
		if err = i.operationEvidence(op); err != nil {
			return nil, err
		}
		if op.PermitID != "" && op.PermitSession != op.PreparedSession {
			return nil, ErrEvidence
		}
		if err = i.assert(`SELECT count(*) FROM users WHERE id=? AND auth_rev<?`, op.UserID, op.AuthRev); err != nil {
			return nil, err
		}
		if err = i.assert(`SELECT count(*) FROM stacks WHERE id=? AND (node_id<>? OR project_name<>?)`, op.StackID, op.NodeID, op.ProjectName); err != nil {
			return nil, err
		}
		if err = i.operationReceipt(op, r.s("idempotency_key")); err != nil {
			return nil, err
		}
		raw, err := s.readPayload(ctx, q, op)
		if err != nil {
			return nil, err
		}
		st, err := readServiceStack(ctx, q, op.StackID)
		if err != nil {
			clear(raw)
			return nil, ErrEvidence
		}
		config, err := s.readConfig(ctx, q, st, op.VersionID)
		if err != nil {
			clear(raw)
			return nil, err
		}
		defer clear(config)
		var payload map[string]json.RawMessage
		if json.Unmarshal(raw, &payload) != nil {
			clear(raw)
			return nil, ErrEvidence
		}
		a, ea := protocol.StrictJSON(payload["config"])
		b, eb := protocol.StrictJSON(config)
		if ea != nil || eb != nil || !reflect.DeepEqual(a, b) {
			clear(raw)
			return nil, ErrEvidence
		}
		return raw, nil
	case "operation_events":
		op, err := s.readOperation(ctx, q, r.s("operation_id"))
		if err != nil {
			return nil, err
		}
		e, err := s.readEvent(ctx, q, op, uint64(r.n("event_seq")))
		if err != nil {
			return nil, err
		}
		p, _ := parseEvent(e.Body)
		if e.Kind == "result" && p.seq == 2 && op.PermitID == "" {
			clear(e.Body)
			return nil, ErrEvidence
		}
		return e.Body, nil
	case "runtime_pending_events":
		op, err := s.readOperation(ctx, q, r.s("operation_id"))
		if err != nil {
			return nil, err
		}
		raw, err := s.unseal(blobBinding{op.OperationID, op.NodeID, op.Digest, "result", uint64(r.n("event_seq"))}, r.blob(t.body))
		if err != nil {
			return nil, err
		}
		fail := func() ([]byte, error) { clear(raw); return nil, ErrEvidence }
		env, err := s.validator.Validate(bodyEnvelope("result", raw))
		if err != nil {
			return fail()
		}
		p, err := parseEvent(env.Body)
		if err != nil || p.OperationID != op.OperationID || p.Digest != op.Digest || p.seq != uint64(r.n("event_seq")) || p.seq < 3 || p.Outcome != "normal_exit" || op.PermitID == "" || p.seq <= op.LatestResultSeq || op.ReleaseStatus == "released" {
			return fail()
		}
		n, err := i.count(`SELECT count(*) FROM runtime_pending_events WHERE operation_id=?`, op.OperationID)
		if err != nil {
			clear(raw)
			return nil, err
		}
		if n > 32 {
			return fail()
		}
		n, err = i.count(`SELECT count(*) FROM operation_events WHERE operation_id=? AND event_seq=?`, op.OperationID, p.seq)
		if err != nil {
			clear(raw)
			return nil, err
		}
		if n != 0 {
			return fail()
		}
		// A predecessor can commit before the separate promotion step. Its new
		// content need not make this retained pending evidence promotable; only
		// the normal receive path may apply or quarantine that comparison.
		return raw, nil
	case "recovery_pages":
		scan, err := s.readRecoveryScanByID(ctx, q, r.s("round_id"))
		if err != nil {
			return nil, err
		}
		req, err := s.readRecoveryRequest(ctx, q, scan, r.s("request_id"))
		if err != nil {
			return nil, err
		}
		return s.unseal(recoveryPageBinding(scan, int(r.n("page_no")), req.Cursor), r.blob(t.body))
	case "recovery_cases":
		c, raw, err := s.readRecoveryCase(ctx, q, r.s("id"))
		if err != nil {
			return nil, err
		}
		if err = i.caseEvidence(c, raw); err != nil {
			clear(raw)
			return nil, err
		}
		return raw, nil
	case "finalization":
		op, err := s.readOperation(ctx, q, r.s("operation_id"))
		if err != nil {
			return nil, err
		}
		return s.readFinalized(ctx, q, op)
	case "user_sessions":
		raw, err := s.unseal(blobBinding{r.s("id"), r.s("user_id"), base64.RawURLEncoding.EncodeToString(r.b("token_hash")), "session-csrf", 0}, r.blob(t.body))
		if err != nil {
			return nil, err
		}
		if !validDigest(string(raw)) {
			clear(raw)
			return nil, ErrEvidence
		}
		return raw, nil
	case "config_versions":
		st, err := readServiceStack(ctx, q, r.s("stack_id"))
		if err != nil {
			return nil, ErrEvidence
		}
		return s.readConfig(ctx, q, st, r.s("id"))
	case "runtime_deliveries":
		return i.delivery(r)
	}
	return nil, nil
}

func (i *snapshotInspector) clearRow(table string, r inspectionRow) error {
	// IDs are schema strings in many clear tables; their format is a business
	// invariant, not implied by SQLite's TEXT type or foreign key enforcement.
	for _, name := range []string{"id", "operation_id", "user_id", "node_id", "stack_id", "version_id", "created_by", "message_id", "request_id", "round_id", "owner_boot", "prepared_session", "permit_id", "permit_session", "start_nonce", "manager_boot", "agent_boot", "scan_id", "last_operation", "active_round", "current_version_id", "last_applied_version_id", "last_operation_id", "observation_operation_id", "parent_id", "session_id", "boot_id", "ledger_id", "idempotency_key"} {
		v, ok := r[name]
		if !ok || v == nil {
			continue
		}
		text, ok := v.(string)
		if !ok {
			return ErrEvidence
		}
		empty := name == "permit_id" || name == "permit_session" || name == "start_nonce" || name == "active_round" || name == "scan_id" || name == "last_operation" || name == "round_id" && table == "runtime_deliveries" || name == "round_id" && table == "finalize_requests" || name == "round_id" && table == "recovery_cases" || name == "ledger_id" && (table == "nodes" || table == "enrollments") || name == "session_id" && table == "nodes" || name == "boot_id" && table == "nodes" || name == "agent_boot" && table == "recovery_nodes"
		if !inspectionID(text, empty) {
			return ErrEvidence
		}
	}
	switch table {
	case "users":
		if !usernamePattern.MatchString(r.s("login_name")) || !inspectionPassword(r.s("password_hash")) {
			return ErrEvidence
		}
	case "nodes":
		return i.node(r)
	case "grants":
		if !validCapability(r.s("capability")) {
			return ErrEvidence
		}
	case "resource_claims":
		op, err := i.view.readOperation(i.ctx, i.tx, r.s("operation_id"))
		if err != nil {
			return err
		}
		if r.s("node_id") != op.NodeID {
			return ErrEvidence
		}
	case "event_conflicts":
		op, err := i.view.readOperation(i.ctx, i.tx, r.s("operation_id"))
		if err != nil {
			return err
		}
		if len(r.b("fingerprint")) != 32 || !op.Blocked || op.ReleaseStatus != "held" {
			return ErrEvidence
		}
		return i.assert(`SELECT count(*) FROM nodes WHERE id=? AND frozen=0`, op.NodeID)
	case "recovery_nodes":
		if r.n("quarantined") == 1 {
			if err := i.assert(`SELECT count(*) FROM nodes WHERE id=? AND frozen=0`, r.s("node_id")); err != nil {
				return err
			}
		}
		if r.s("active_round") != "" {
			scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, r.s("active_round"))
			if err != nil {
				return err
			}
			if scan.NodeID != r.s("node_id") {
				return ErrEvidence
			}
		}
	case "recovery_scans":
		return i.scan(r.s("id"))
	case "recovery_requests":
		scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, r.s("round_id"))
		if err != nil {
			return err
		}
		if r.s("kind") == "reconcile" {
			return i.view.validateRecoveryRequest(scan, recoveryRequest{r.s("message_id"), r.s("cursor"), int(r.n("page_no")), r.b("raw"), r.n("accepted") == 1})
		}
		_, id, _, err := i.view.recoveryCompleteRequest(i.ctx, i.tx, scan)
		if err != nil {
			return err
		}
		if id != r.s("message_id") {
			return ErrEvidence
		}
	case "absence_releases":
		op, err := i.view.readOperation(i.ctx, i.tx, r.s("operation_id"))
		if err != nil {
			return err
		}
		return i.view.verifyAbsenceRelease(i.ctx, i.tx, op)
	case "recovery_expected":
		op, err := i.view.readOperation(i.ctx, i.tx, r.s("operation_id"))
		if err != nil {
			return err
		}
		scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, r.s("round_id"))
		if err != nil {
			return err
		}
		if op.NodeID != scan.NodeID || op.LedgerID != scan.LedgerID || uint64(r.n("last_event_seq")) > op.MaxEventSeq {
			return ErrEvidence
		}
	case "finalize_requests":
		return i.finalizeRequest(r)
	case "user_sessions":
		return i.assert(`SELECT count(*) FROM users WHERE id=? AND auth_rev<?`, r.s("user_id"), r.n("auth_rev"))
	case "node_credentials":
		if len(r.b("token_hash")) != 32 || r.n("generation") > 9007199254740991 {
			return ErrEvidence
		}
		return i.assert(`SELECT count(*) FROM nodes WHERE id=? AND (credential_generation<? OR (?=1 AND (credential_generation<>? OR revoked_at IS NOT NULL)))`, r.s("node_id"), r.n("generation"), r.n("enabled"), r.n("generation"))
	case "enrollments":
		if len(r.b("token_hash")) != 32 {
			return ErrEvidence
		}
		if r["consumed_at"] != nil {
			n, err := i.count(`SELECT count(*) FROM node_credentials c JOIN nodes n ON n.id=c.node_id WHERE c.node_id=? AND c.generation=1 AND c.token_hash=? AND n.ledger_id=?`, r.s("node_id"), r.b("candidate_hash"), r.s("ledger_id"))
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrEvidence
			}
		}
	case "stacks":
		return i.stack(r)
	case "config_versions":
		if r["parent_id"] != nil && r.s("parent_id") == r.s("id") {
			return ErrEvidence
		}
	case "idempotency_records":
		return i.receipt(r)
	case "audit_events":
		if r["user_id"] == nil && r["node_id"] == nil {
			return ErrEvidence
		}
		if !inspectionID(r.s("target_id"), false) {
			return ErrEvidence
		}
		if r["operation_id"] != nil && r.s("operation_id") != "" {
			n, err := i.count(`SELECT count(*) FROM operations WHERE operation_id=?`, r.s("operation_id"))
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrEvidence
			}
		}
	}
	return nil
}

func inspectionPassword(encoded string) bool {
	p := strings.Split(encoded, "$")
	if len(p) != 6 || len(encoded) != 97 || p[1] != "argon2id" || p[2] != "v=19" || p[3] != "m=65536,t=3,p=1" {
		return false
	}
	a, e := base64.RawStdEncoding.Strict().DecodeString(p[4])
	if e != nil || len(a) != 16 {
		return false
	}
	b, e := base64.RawStdEncoding.Strict().DecodeString(p[5])
	return e == nil && len(b) == 32
}
func (i *snapshotInspector) node(r inspectionRow) error {
	if r.s("display_name") == "" || len(r.s("display_name")) > 512 || strings.ContainsAny(r.s("display_name"), "\x00\r\n") {
		return ErrEvidence
	}
	gen := r.n("credential_generation")
	var max sql.NullInt64
	var enabled int
	if err := i.tx.QueryRowContext(i.ctx, `SELECT max(generation),coalesce(sum(enabled),0) FROM node_credentials WHERE node_id=?`, r.s("id")).Scan(&max, &enabled); err != nil {
		return ErrStorage
	}
	if gen == 0 {
		if max.Valid || r.s("ledger_id") != "" {
			return ErrEvidence
		}
	} else if !max.Valid || max.Int64 != gen || !validID(r.s("ledger_id")) {
		return ErrEvidence
	}
	if enabled > 1 || r["revoked_at"] != nil && enabled != 0 {
		return ErrEvidence
	}
	if gen > 0 && r["revoked_at"] == nil && enabled != 1 {
		return ErrEvidence
	}
	if (r.s("session_id") == "") != (r.s("boot_id") == "") {
		return ErrEvidence
	}
	caps, err := protocol.StrictJSON([]byte(r.s("agent_capabilities_json")))
	if err != nil {
		return ErrEvidence
	}
	list, ok := caps.([]any)
	if !ok {
		return ErrEvidence
	}
	for _, v := range list {
		if _, ok := v.(string); !ok {
			return ErrEvidence
		}
	}
	if r["execution_profile_json"] != nil {
		raw, err := json.Marshal(map[string]any{"protocol": 1, "type": "hello", "message_id": validationID, "session_id": nil, "reply_to": nil, "body": map[string]any{"node_id": r.s("id"), "ledger_id": r.s("ledger_id"), "boot_id": validationID, "agent_version": "inspection", "compose_version": "inspection", "capabilities": caps, "execution_profile": json.RawMessage(r.s("execution_profile_json"))}})
		if err != nil {
			return ErrEvidence
		}
		if _, err = i.view.validator.Validate(raw); err != nil {
			return ErrEvidence
		}
	}
	return nil
}
func (i *snapshotInspector) scan(id string) error {
	scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, id)
	if err != nil {
		return err
	}
	if err = i.assert(`SELECT count(*) FROM nodes WHERE id=? AND ledger_id<>?`, scan.NodeID, scan.LedgerID); err != nil {
		return err
	}
	pages, err := i.view.readRecoveryPages(i.ctx, i.tx, scan)
	if err != nil {
		return err
	}
	defer clearRecoveryPages(pages)
	complete := scan.Pages > 0 && scan.Total >= 0 && scan.Seen == scan.Total && scan.NextCursor == ""
	if scan.Status == "blocked" && !complete {
		return ErrEvidence
	}
	raw, _, accepted, e := i.view.recoveryCompleteRequest(i.ctx, i.tx, scan)
	clear(raw)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return e
	}
	if scan.Status == "completed" && (e != nil || !accepted) || scan.Status == "completing" && (e != nil || accepted) {
		return ErrEvidence
	}
	if e == nil && !complete {
		return ErrEvidence
	}
	if accepted && scan.Status != "completed" && scan.Status != "abandoned" {
		return ErrEvidence
	}
	return nil
}
func (i *snapshotInspector) operationReceipt(op Operation, key string) error {
	var typ, id, method, path, body, location, keyID string
	var status int
	err := i.tx.QueryRowContext(i.ctx, `SELECT result_type,result_id,method,path,response_status,safe_response_json,response_location,digest_key_id FROM idempotency_records WHERE user_id=? AND idempotency_key=?`, op.UserID, key).Scan(&typ, &id, &method, &path, &status, &body, &location, &keyID)
	if err != nil {
		return ErrEvidence
	}
	if typ != "operation" || id != op.OperationID || method != "POST" || path != "/api/v1/stacks/"+op.StackID+"/apply" || status != 202 || location != "/api/v1/operations/"+op.OperationID || keyID != i.view.identity.DigestKeyID {
		return ErrEvidence
	}
	doc, err := protocol.StrictJSON([]byte(body))
	if err != nil {
		return ErrEvidence
	}
	m, ok := doc.(map[string]any)
	if !ok || len(m) != 2 || m["operation_id"] != op.OperationID || m["state"] != "accepted" {
		return ErrEvidence
	}
	return nil
}
func (i *snapshotInspector) receipt(r inspectionRow) error {
	if r.s("digest_key_id") != i.view.identity.DigestKeyID || len(r.b("request_digest")) != 32 || !validID(r.s("result_id")) {
		return ErrEvidence
	}
	if _, err := protocol.StrictJSON([]byte(r.s("safe_response_json"))); err != nil {
		return ErrEvidence
	}
	var table, col string
	switch r.s("result_type") {
	case "user", "grant":
		table, col = "users", "id"
	case "node":
		table, col = "nodes", "id"
	case "enrollment":
		table, col = "enrollments", "id"
	case "stack":
		table, col = "stacks", "id"
	case "version":
		table, col = "config_versions", "id"
	case "operation":
		op, err := i.view.readOperation(i.ctx, i.tx, r.s("result_id"))
		if err != nil {
			return err
		}
		if op.UserID != r.s("user_id") {
			return ErrEvidence
		}
		var key string
		if err = i.tx.QueryRowContext(i.ctx, `SELECT idempotency_key FROM operations WHERE operation_id=?`, op.OperationID).Scan(&key); err != nil {
			return ErrStorage
		}
		if key != r.s("idempotency_key") {
			return ErrEvidence
		}
		return i.operationReceipt(op, key)
	default:
		return ErrEvidence
	}
	n, err := i.count("SELECT count(*) FROM "+table+" WHERE "+col+"=?", r.s("result_id"))
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrEvidence
	}
	return nil
}

func (i *snapshotInspector) stack(r inspectionRow) error {
	id := r.s("id")
	n, err := i.count(`SELECT count(*) FROM config_versions WHERE stack_id=?`, id)
	if err != nil {
		return err
	}
	roots, err := i.count(`SELECT count(*) FROM config_versions WHERE stack_id=? AND parent_id IS NULL`, id)
	if err != nil {
		return err
	}
	if n != r.n("revision") || roots != 1 {
		return ErrEvidence
	}
	if err = i.assert(`SELECT count(*) FROM (SELECT parent_id FROM config_versions WHERE stack_id=? AND parent_id IS NOT NULL GROUP BY parent_id HAVING count(*)>1)`, id); err != nil {
		return err
	}
	current := r.s("current_version_id")
	steps := int64(0)
	for current != "" {
		if err = i.ctx.Err(); err != nil {
			return err
		}
		steps++
		if steps > n {
			return ErrEvidence
		}
		var parent sql.NullString
		if err = i.tx.QueryRowContext(i.ctx, `SELECT parent_id FROM config_versions WHERE id=? AND stack_id=?`, current, id).Scan(&parent); err != nil {
			return ErrEvidence
		}
		current = parent.String
	}
	if steps != n {
		return ErrEvidence
	}
	if r["last_operation_id"] != nil {
		op, err := i.view.readOperation(i.ctx, i.tx, r.s("last_operation_id"))
		if err != nil {
			return err
		}
		if op.StackID != id || op.NodeID != r.s("node_id") {
			return ErrEvidence
		}
	}
	if r["observation_operation_id"] != nil {
		op, err := i.view.readOperation(i.ctx, i.tx, r.s("observation_operation_id"))
		if err != nil {
			return err
		}
		if op.StackID != id || op.NodeID != r.s("node_id") || op.LatestResultSeq == 0 {
			return ErrEvidence
		}
		e, err := i.view.readEvent(i.ctx, i.tx, op, op.LatestResultSeq)
		if err != nil {
			return err
		}
		p, _ := parseEvent(e.Body)
		clear(e.Body)
		if p.Observation == nil {
			return ErrEvidence
		}
	}
	if r["last_applied_version_id"] != nil {
		// A later conflict can block the operation without erasing its earlier
		// authenticated successful result. Search retained original results.
		found := false
		var last string
		for {
			var opid string
			err = i.tx.QueryRowContext(i.ctx, `SELECT operation_id FROM operations WHERE stack_id=? AND version_id=? AND operation_id>? ORDER BY operation_id LIMIT 1`, id, r.s("last_applied_version_id"), last).Scan(&opid)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return ErrStorage
			}
			last = opid
			op, err := i.view.readOperation(i.ctx, i.tx, opid)
			if err != nil {
				return err
			}
			if op.LatestResultSeq == 0 {
				continue
			}
			e, err := i.view.readEvent(i.ctx, i.tx, op, op.LatestResultSeq)
			if err != nil {
				return err
			}
			p, _ := parseEvent(e.Body)
			clear(e.Body)
			state, _ := resultProjection(p)
			if state == "succeeded" {
				found = true
				break
			}
		}
		if !found {
			return ErrEvidence
		}
	}
	return nil
}

func (i *snapshotInspector) finalizeRequest(row inspectionRow) error {
	op, err := i.view.readOperation(i.ctx, i.tx, row.s("operation_id"))
	if err != nil {
		return err
	}
	r := finalizeRequest{ID: row.s("message_id"), OperationID: op.OperationID, NodeID: row.s("node_id"), LedgerID: row.s("ledger_id"), SessionID: row.s("session_id"), ManagerBoot: row.s("manager_boot"), AgentBoot: row.s("agent_boot"), RoundID: row.s("round_id"), Raw: row.b("raw"), Accepted: row.n("accepted") == 1, ConfirmationMAC: row.b("confirmation_mac")}
	env, err := i.view.validator.Validate(r.Raw)
	if err != nil || env.Type != "finalize" || env.MessageID != r.ID || env.SessionID == nil || *env.SessionID != r.SessionID || r.NodeID != op.NodeID || r.LedgerID != op.LedgerID {
		return ErrEvidence
	}
	body, err := parseFinalization(env.Body)
	if err != nil || body.OperationID != op.OperationID || body.Digest != op.Digest {
		return ErrEvidence
	}
	e, err := i.view.readEvent(i.ctx, i.tx, op, body.ResultSeq)
	if err != nil {
		return err
	}
	p, _ := parseEvent(e.Body)
	clear(e.Body)
	state, release := resultProjection(p)
	if e.Kind != "result" || state != body.State || release != "pending" {
		return ErrEvidence
	}
	if r.RoundID != "" {
		scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, r.RoundID)
		if err != nil {
			return err
		}
		if scan.NodeID != r.NodeID || scan.LedgerID != r.LedgerID || scan.SessionID != r.SessionID || scan.ManagerBoot != r.ManagerBoot || scan.AgentBootID != r.AgentBoot {
			return ErrEvidence
		}
	}
	if r.Accepted {
		if op.ReleaseStatus != "released" || body.ResultSeq != op.LatestResultSeq || len(r.ConfirmationMAC) != 32 {
			return ErrEvidence
		}
		mac, err := i.view.finalizeConfirmationMAC(r, body)
		if err != nil {
			return err
		}
		if !hmac.Equal(mac, r.ConfirmationMAC) {
			return ErrEvidence
		}
	} else if len(r.ConfirmationMAC) != 0 {
		return ErrEvidence
	}
	return nil
}

func (i *snapshotInspector) delivery(r inspectionRow) ([]byte, error) {
	op, err := i.view.readOperation(i.ctx, i.tx, r.s("operation_id"))
	if err != nil {
		return nil, err
	}
	b := Binding{r.s("node_id"), r.s("ledger_id"), r.s("session_id")}
	d := Delivery{MessageID: r.s("message_id"), OperationID: op.OperationID, Kind: r.s("kind"), Generation: uint64(r.n("generation"))}
	if b.NodeID != op.NodeID || b.LedgerID != op.LedgerID {
		return nil, ErrEvidence
	}
	raw, err := i.view.unseal(deliveryBinding(b, d, r.s("manager_boot"), r.s("round_id")), r.blob("body_cipher"))
	if err != nil {
		return nil, err
	}
	fail := func() ([]byte, error) { clear(raw); return nil, ErrEvidence }
	env, err := i.view.validator.Validate(raw)
	if err != nil || env.Type != d.Kind || env.MessageID != d.MessageID || env.SessionID == nil || *env.SessionID != b.SessionID {
		return fail()
	}
	if r.s("round_id") != "" {
		scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, r.s("round_id"))
		if err != nil {
			clear(raw)
			return nil, err
		}
		if scan.NodeID != b.NodeID || scan.LedgerID != b.LedgerID || scan.SessionID != b.SessionID || scan.ManagerBoot != r.s("manager_boot") {
			return fail()
		}
	}
	code := r.s("failure_code")
	switch code {
	case "", "write_failed", "connection_closed", "queue_full", "timeout":
	case "cursor_expired", "observation_failed":
		if d.Kind != "observe" {
			return fail()
		}
	default:
		return fail()
	}
	if d.Kind != "observe" && d.Generation != 1 {
		return fail()
	}
	switch d.Kind {
	case "prepare":
		var p prepareFields
		if json.Unmarshal(env.Body, &p) != nil || p.Operation != op.OperationID || p.Digest != op.Digest || p.Node != op.NodeID || p.Stack != op.StackID || p.Version != op.VersionID {
			return fail()
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(p.Payload)
		if err != nil {
			return fail()
		}
		defer clear(decoded)
		original, err := i.view.readPayload(i.ctx, i.tx, op)
		if err != nil {
			clear(raw)
			return nil, err
		}
		same := bytes.Equal(decoded, original)
		clear(original)
		if !same {
			return fail()
		}
	case "cancel":
		var p struct {
			ID     string `json:"operation_id"`
			Digest string `json:"input_digest"`
		}
		if json.Unmarshal(env.Body, &p) != nil || p.ID != op.OperationID || p.Digest != op.Digest {
			return fail()
		}
	case "finalize":
		var original []byte
		err := i.tx.QueryRowContext(i.ctx, `SELECT raw FROM finalize_requests WHERE message_id=? AND operation_id=? AND node_id=? AND ledger_id=? AND session_id=? AND manager_boot=? AND round_id=?`, d.MessageID, op.OperationID, b.NodeID, b.LedgerID, b.SessionID, r.s("manager_boot"), r.s("round_id")).Scan(&original)
		if err != nil || !bytes.Equal(original, raw) {
			return fail()
		}
	case "observe":
		var p struct {
			Operation string  `json:"operation_id"`
			Stack     string  `json:"stack_id"`
			Resource  string  `json:"resource_kind"`
			Cursor    *string `json:"cursor"`
			Limit     int     `json:"limit"`
		}
		if json.Unmarshal(env.Body, &p) != nil || p.Operation != op.OperationID || p.Stack != op.StackID || p.Resource != "container" || p.Cursor != nil || p.Limit != 200 {
			return fail()
		}
		event, err := i.view.readEvent(i.ctx, i.tx, op, d.Generation)
		if err != nil {
			clear(raw)
			return nil, err
		}
		parsed, _ := parseEvent(event.Body)
		clear(event.Body)
		if event.Kind != "result" || !observationRepairable(parsed) {
			return fail()
		}
	}
	return raw, nil
}

func (i *snapshotInspector) caseEvidence(c RecoveryCase, raw []byte) error {
	if _, err := protocol.StrictJSON(raw); err != nil {
		return ErrEvidence
	}
	var frozen, quarantined int
	if err := i.tx.QueryRowContext(i.ctx, `SELECT n.frozen,r.quarantined FROM nodes n JOIN recovery_nodes r ON r.node_id=n.id WHERE n.id=?`, c.NodeID).Scan(&frozen, &quarantined); err != nil {
		return ErrEvidence
	}
	if frozen != 1 || quarantined != 1 {
		return ErrEvidence
	}
	if err := i.assert(`SELECT count(*) FROM nodes WHERE id=? AND ledger_id<>?`, c.NodeID, c.LedgerID); err != nil {
		return err
	}
	if err := i.assert(`SELECT count(*) FROM operations WHERE operation_id=? AND node_id=? AND ledger_id=? AND release_status<>'released' AND blocked=0`, c.OperationID, c.NodeID, c.LedgerID); err != nil {
		return err
	}
	if c.RoundID != "" {
		scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, c.RoundID)
		if err != nil {
			return err
		}
		if scan.NodeID != c.NodeID || scan.LedgerID != c.LedgerID {
			return ErrEvidence
		}
	}
	if c.Kind == "missing_history" || c.Kind == "damaged_operation" {
		var reason map[string]any
		if json.Unmarshal(raw, &reason) == nil && len(reason) == 3 && reason["operation_id"] == c.OperationID && (reason["reason"] == "local evidence invalid" || reason["reason"] == "authorized or observed operation absent from all scan") {
			scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, c.RoundID)
			if err != nil {
				return err
			}
			if reason["scan_id"] != scan.ScanID {
				return ErrEvidence
			}
			return nil
		}
	}
	if c.Kind == "page_conflict" {
		if _, err := i.view.validator.Validate(inspectionReplyEnvelope(raw)); err != nil {
			return ErrEvidence
		}
		scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, c.RoundID)
		if err != nil {
			return err
		}
		// The incoming page may contradict the original scan identifiers; that
		// disagreement is precisely why the real receiver retained the case.
		if scan.ScanID == "" || scan.ScanID != c.OperationID {
			return ErrEvidence
		}
		return nil
	}
	if c.RoundID != "" {
		switch c.Kind {
		case "unknown_operation", "damaged_operation", "record_mismatch", "released_record", "agent_reconciliation_required", "started_evidence_conflict", "unsupported_history", "result_conflict":
		default:
			return ErrEvidence
		}
		// Enclosing validation establishes the shared RecoveryRecord Schema.
		body := append([]byte(`{"scope":"all","scan_id":"`+validationID+`","ledger_id":"`+c.LedgerID+`","total":1,"records":[`), raw...)
		body = append(body, []byte(`],"next_cursor":null,"complete":true,"error":null}`)...)
		_, err := i.view.validator.Validate(inspectionReplyEnvelope(body))
		clear(body)
		if err != nil {
			return ErrEvidence
		}
		record, err := parseRecoveryRecord(raw)
		if err != nil || record.OperationID != c.OperationID {
			return ErrEvidence
		}
		var page, number int
		if err = i.tx.QueryRowContext(i.ctx, `SELECT page_no,record_no FROM recovery_members WHERE round_id=? AND operation_id=?`, c.RoundID, c.OperationID).Scan(&page, &number); err != nil {
			return ErrEvidence
		}
		scan, err := i.view.readRecoveryScanByID(i.ctx, i.tx, c.RoundID)
		if err != nil {
			return err
		}
		pages, err := i.view.readRecoveryPages(i.ctx, i.tx, scan)
		if err != nil {
			return err
		}
		defer clearRecoveryPages(pages)
		if page < 0 || page >= len(pages) {
			return ErrEvidence
		}
		parsed, err := parseRecoveryPage(pages[page])
		if err != nil || number < 0 || number >= len(parsed.Records) || !bytes.Equal(parsed.Records[number], raw) {
			return ErrEvidence
		}
		return nil
	}
	switch c.Kind {
	case "unknown_event", "damaged_operation", "event_mismatch", "released_event", "unsupported_event", "event_conflict":
	default:
		return ErrEvidence
	}
	for _, kind := range []string{"started", "result"} {
		env, err := i.view.validator.Validate(bodyEnvelope(kind, raw))
		if err == nil {
			p, err := parseEvent(env.Body)
			if err == nil && p.OperationID == c.OperationID {
				return nil
			}
		}
	}
	return ErrEvidence
}

func inspectionReplyEnvelope(body []byte) []byte {
	return bytes.Replace(bodyEnvelope("reconcile_reply", body), []byte(`"reply_to":null`), []byte(`"reply_to":"`+validationID+`"`), 1)
}
