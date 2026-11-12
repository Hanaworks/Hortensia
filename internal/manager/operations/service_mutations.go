package operations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"docker-manager/internal/protocol"
)

type serviceRoute struct{ kind, node, stack, version, user, operation, key string }

func parseServiceRoute(r ServiceRequest) (serviceRoute, error) {
	p := strings.Split(r.Path, "/")
	rt := serviceRoute{}
	if len(p) < 4 || p[0] != "" || p[1] != "api" || p[2] != "v1" {
		return rt, serviceError(404, "not_found")
	}
	suffix := p[3:]
	switch suffix[0] {
	case "users":
		if len(suffix) == 1 {
			rt.kind = "users"
		} else if len(suffix) == 2 {
			rt.kind = "user"
			rt.user = suffix[1]
		}
	case "nodes":
		if len(suffix) == 1 {
			rt.kind = "nodes"
		} else {
			rt.node = suffix[1]
			if len(suffix) == 2 {
				rt.kind = "node"
			} else if len(suffix) == 3 {
				switch suffix[2] {
				case "grants", "stacks", "enrollment-tokens", "revoke":
					rt.kind = suffix[2]
				}
			} else if len(suffix) == 4 && suffix[2] == "grants" {
				rt.kind = "grant"
				rt.user = suffix[3]
			}
		}
	case "stacks":
		if len(suffix) >= 2 {
			rt.stack = suffix[1]
			if len(suffix) == 2 {
				rt.kind = "stack"
			} else if len(suffix) == 3 && (suffix[2] == "versions" || suffix[2] == "apply") {
				rt.kind = suffix[2]
			} else if len(suffix) == 4 && suffix[2] == "versions" {
				rt.kind = "version"
				rt.version = suffix[3]
			}
		}
	case "operations":
		if len(suffix) == 1 {
			rt.kind = "operations"
			rt.node = r.NodeID
		} else if len(suffix) == 2 {
			rt.kind = "operation"
			rt.operation = suffix[1]
		} else if len(suffix) == 3 && (suffix[2] == "cancel" || suffix[2] == "output") {
			rt.kind = suffix[2]
			rt.operation = suffix[1]
		}
	case "submissions":
		if len(suffix) == 2 {
			rt.kind = "submission"
			rt.key = suffix[1]
		}
	}
	if rt.kind == "" {
		return rt, serviceError(404, "not_found")
	}
	for _, id := range []string{rt.node, rt.stack, rt.version, rt.user, rt.operation, rt.key} {
		if id != "" && !validID(id) {
			return rt, serviceError(400, "invalid_field")
		}
	}
	if rt.kind == "operations" && rt.node == "" {
		return rt, serviceError(400, "invalid_field")
	}
	return rt, nil
}

type serviceBody struct {
	Username       string          `json:"username"`
	Password       string          `json:"password"`
	Enabled        *bool           `json:"enabled"`
	DisplayName    string          `json:"display_name"`
	Capabilities   []string        `json:"capabilities"`
	ReplacePending bool            `json:"replace_pending"`
	ProjectName    string          `json:"project_name"`
	Config         json.RawMessage `json:"config"`
	VersionID      string          `json:"version_id"`
}

func parseServiceBody(r ServiceRequest, rt serviceRoute) (serviceBody, any, error) {
	var b serviceBody
	raw := r.Body
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	v, e := protocol.StrictJSON(raw)
	if e != nil {
		return b, nil, serviceError(400, "invalid_json")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return b, nil, serviceError(400, "invalid_field")
	}
	allowed, required := "", ""
	if r.Method != "GET" {
		switch rt.kind {
		case "users":
			allowed = "username password"
			required = allowed
		case "user":
			allowed = "enabled password"
			if len(m) != 1 {
				return b, nil, serviceError(400, "invalid_field")
			}
		case "nodes":
			allowed = "display_name"
			required = allowed
		case "grant":
			allowed = "capabilities"
			required = allowed
		case "enrollment-tokens":
			allowed = "replace_pending"
		case "stacks":
			allowed = "project_name config"
			required = allowed
		case "versions":
			allowed = "config"
			required = allowed
		case "apply":
			allowed = "version_id"
			required = allowed
		}
	}
	allowedKeys := map[string]bool{}
	for _, key := range strings.Fields(allowed) {
		allowedKeys[key] = true
	}
	for k, val := range m {
		if !allowedKeys[k] || val == nil {
			return b, nil, serviceError(400, "invalid_field")
		}
	}
	for _, k := range strings.Fields(required) {
		if _, ok := m[k]; !ok {
			return b, nil, serviceError(400, "invalid_field")
		}
	}
	if e = json.Unmarshal(raw, &b); e != nil {
		return b, nil, serviceError(400, "invalid_field")
	}
	if rt.kind == "enrollment-tokens" && r.Method == "POST" {
		m["replace_pending"] = b.ReplacePending
	}
	if x, ok := m["username"]; ok && (!usernamePattern.MatchString(b.Username) || x == nil) {
		return b, nil, serviceError(400, "invalid_field")
	}
	if _, ok := m["password"]; ok && !validPassword(b.Password) {
		return b, nil, serviceError(400, "invalid_field")
	}
	if _, ok := m["display_name"]; ok && (!utf8.ValidString(b.DisplayName) || utf8.RuneCountInString(b.DisplayName) < 1 || utf8.RuneCountInString(b.DisplayName) > 128 || strings.ContainsAny(b.DisplayName, "\x00\r\n")) {
		return b, nil, serviceError(400, "invalid_field")
	}
	if _, ok := m["project_name"]; ok && !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`).MatchString(b.ProjectName) {
		return b, nil, serviceError(400, "invalid_field")
	}
	if _, ok := m["version_id"]; ok && !validID(b.VersionID) {
		return b, nil, serviceError(400, "invalid_field")
	}
	seen := map[string]bool{}
	for _, c := range b.Capabilities {
		if !validCapability(c) || seen[c] {
			return b, nil, serviceError(400, "invalid_field")
		}
		seen[c] = true
	}
	return b, m, nil
}
func (s *Store) requestDigest(r ServiceRequest, body any) ([]byte, error) {
	var condition any
	if r.IfMatch != "" {
		if len(r.IfMatch) < 3 || r.IfMatch[0] != '"' || r.IfMatch[len(r.IfMatch)-1] != '"' || strings.Contains(r.IfMatch[1:len(r.IfMatch)-1], `"`) {
			return nil, serviceError(400, "invalid_field")
		}
		condition = r.IfMatch[1 : len(r.IfMatch)-1]
	}
	raw, e := json.Marshal([]any{r.Method, r.Path, condition, body})
	if e != nil {
		return nil, ErrInput
	}
	keys, e := s.keyCopies()
	if e != nil {
		return nil, e
	}
	defer clear(keys.DigestKey)
	defer clear(keys.EncryptionKey)
	h := hmac.New(sha256.New, keys.DigestKey)
	h.Write([]byte("dm-http-request-v1\x00"))
	h.Write(raw)
	return h.Sum(nil), nil
}
func requireAdmin(u serviceUser) error {
	if !u.admin {
		return serviceError(403, "permission_denied")
	}
	return nil
}
func requireCaps(ctx context.Context, q querier, u serviceUser, node string, caps ...string) error {
	var exists int
	if e := q.QueryRowContext(ctx, `SELECT count(*) FROM nodes WHERE id=?`, node).Scan(&exists); e != nil {
		return serviceStorageError(e)
	}
	if exists != 1 {
		return serviceError(404, "not_found")
	}
	for _, cap := range caps {
		var n int
		if e := q.QueryRowContext(ctx, `SELECT count(*) FROM grants WHERE user_id=? AND node_id=? AND capability=?`, u.id, node, cap).Scan(&n); e != nil {
			return serviceStorageError(e)
		}
		if n != 1 {
			if cap == "node.read" {
				return serviceError(404, "not_found")
			}
			return serviceError(403, "permission_denied")
		}
	}
	return nil
}
func checkETag(got, want string) error {
	if got == "" {
		return serviceError(428, "precondition_required")
	}
	if len(got) < 3 || got[0] != '"' || got[len(got)-1] != '"' || strings.Contains(got[1:len(got)-1], `"`) {
		return serviceError(400, "invalid_field")
	}
	if got != `"`+want+`"` {
		return serviceError(412, "version_conflict")
	}
	return nil
}

type preparedService struct {
	id, version, node, stack, hash, secret string
	sealed                                 *sealedBlob
	payload                                []byte
	operation                              Operation
	binding                                Binding
	ready                                  bool
}

var errPrepareService = errors.New("prepare service mutation")

func (s *Store) DoService(ctx context.Context, browser *BrowserSession, r ServiceRequest) (ServiceReply, error) {
	rt, e := parseServiceRoute(r)
	if e != nil {
		return ServiceReply{}, e
	}
	body, canonical, e := parseServiceBody(r, rt)
	if e != nil {
		return ServiceReply{}, e
	}
	if len(body.Config) > 0 {
		if e = s.validator.ValidateConfig(body.Config); e != nil {
			return ServiceReply{}, serviceError(400, "invalid_field")
		}
	}
	isWrite := r.Method != "GET"
	if isWrite && rt.kind != "cancel" && !validID(r.IdempotencyKey) {
		return ServiceReply{}, serviceError(400, "invalid_field")
	}
	digest, e := s.requestDigest(r, canonical)
	if e != nil {
		return ServiceReply{}, e
	}
	p := &preparedService{}
	for attempt := 0; attempt < 2; attempt++ {
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return ServiceReply{}, serviceStorageError(e)
		}
		u, e := s.requireBrowserTx(ctx, tx, browser)
		if e != nil {
			tx.Rollback()
			return ServiceReply{}, e
		}
		if !isWrite {
			reply, e := s.queryService(ctx, tx, u, r, rt)
			tx.Rollback()
			return reply, e
		}
		if rt.kind != "cancel" {
			reply, found, e := s.replayService(ctx, tx, u, r, digest)
			if e != nil || found {
				tx.Rollback()
				return reply, e
			}
		}
		reply, resultType, resultID, e := s.mutateService(ctx, tx, u, r, rt, body, p)
		if errors.Is(e, errPrepareService) {
			tx.Rollback()
			p.id, e = protocol.NewID()
			if e != nil {
				return ServiceReply{}, serviceStorageError(e)
			}
			if len(body.Config) > 0 {
				p.version, e = protocol.NewID()
				if e != nil {
					return ServiceReply{}, serviceStorageError(e)
				}
				if rt.kind == "stacks" {
					p.stack = p.id
				}
				sealed, e := s.seal(ctx, blobBinding{OperationID: p.version, NodeID: p.node, Digest: p.stack, Kind: "config"}, body.Config)
				if e != nil {
					return ServiceReply{}, e
				}
				p.sealed = &sealed
			}
			if body.Password != "" {
				p.hash, e = hashPassword(ctx, body.Password)
				if e != nil {
					return ServiceReply{}, e
				}
			}
			if rt.kind == "enrollment-tokens" {
				p.secret, e = randomSecret()
				if e != nil {
					return ServiceReply{}, e
				}
			}
			if rt.kind == "apply" {
				sealed, e := s.seal(ctx, blobBinding{OperationID: p.id, NodeID: p.node, Digest: p.operation.Digest, Kind: "payload"}, p.payload)
				if e != nil {
					return ServiceReply{}, e
				}
				p.sealed = &sealed
			}
			p.ready = true
			continue
		}
		if e != nil {
			tx.Rollback()
			return ServiceReply{}, e
		}
		safe := reply
		if resultType == "enrollment" {
			var m map[string]any
			json.Unmarshal(reply.Body, &m)
			m["token"] = nil
			safe = serviceReply(reply.Status, m)
		}
		if rt.kind != "cancel" {
			_, e = tx.ExecContext(ctx, `INSERT INTO idempotency_records(user_id,idempotency_key,method,path,request_digest,digest_key_id,result_type,result_id,response_status,safe_response_json,response_etag,response_location,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, u.id, r.IdempotencyKey, r.Method, r.Path, digest, s.identity.DigestKeyID, resultType, resultID, safe.Status, string(safe.Body), safe.ETag, safe.Location, time.Now().UnixMilli())
			if e != nil {
				tx.Rollback()
				return ServiceReply{}, serviceStorageError(e)
			}
		}
		audit, e := protocol.NewID()
		if e == nil {
			_, e = tx.ExecContext(ctx, `INSERT INTO audit_events(id,user_id,action,target_type,target_id,created_at,outcome) VALUES(?,?,?,?,?,?,'committed')`, audit, u.id, r.Method+" "+rt.kind, resultType, resultID, time.Now().UnixMilli())
		}
		if e != nil {
			tx.Rollback()
			return ServiceReply{}, serviceStorageError(e)
		}
		if e = tx.Commit(); e != nil {
			return ServiceReply{}, serviceStorageError(e)
		}
		return reply, nil
	}
	return ServiceReply{}, serviceStorageError(e)
}
func (s *Store) replayService(ctx context.Context, tx *sql.Tx, u serviceUser, r ServiceRequest, digest []byte) (ServiceReply, bool, error) {
	var method, path, typ, id, body, keyID string
	var old []byte
	var reply ServiceReply
	e := tx.QueryRowContext(ctx, `SELECT method,path,request_digest,digest_key_id,result_type,result_id,response_status,safe_response_json,response_etag,response_location FROM idempotency_records WHERE user_id=? AND idempotency_key=?`, u.id, r.IdempotencyKey).Scan(&method, &path, &old, &keyID, &typ, &id, &reply.Status, &body, &reply.ETag, &reply.Location)
	if errors.Is(e, sql.ErrNoRows) {
		return reply, false, nil
	}
	if e != nil {
		return reply, false, serviceStorageError(e)
	}
	if e = s.receiptPermission(ctx, tx, u, typ, id, path); e != nil {
		return reply, false, e
	}
	if method != r.Method || path != r.Path || keyID != s.identity.DigestKeyID || !hmac.Equal(old, digest) {
		return reply, false, serviceError(409, "idempotency_conflict")
	}
	reply.Body = json.RawMessage(body)
	if typ == "enrollment" {
		var m map[string]any
		if json.Unmarshal(reply.Body, &m) != nil {
			return reply, false, ErrEvidence
		}
		state, e := enrollmentStatus(ctx, tx, id)
		if e != nil {
			return reply, false, e
		}
		m["status"] = state
		reply.Body, _ = json.Marshal(m)
	}
	return reply, true, nil
}
func (s *Store) receiptPermission(ctx context.Context, tx *sql.Tx, u serviceUser, typ, id, path string) error {
	if typ == "user" || typ == "grant" || typ == "node" || typ == "enrollment" {
		return requireAdmin(u)
	}
	var node string
	var e error
	switch typ {
	case "stack":
		e = tx.QueryRowContext(ctx, `SELECT node_id FROM stacks WHERE id=?`, id).Scan(&node)
	case "version":
		e = tx.QueryRowContext(ctx, `SELECT s.node_id FROM config_versions v JOIN stacks s ON s.id=v.stack_id WHERE v.id=?`, id).Scan(&node)
	case "operation":
		e = tx.QueryRowContext(ctx, `SELECT node_id FROM operations WHERE operation_id=?`, id).Scan(&node)
	default:
		return ErrEvidence
	}
	if e != nil {
		return ErrEvidence
	}
	if e = requireCaps(ctx, tx, u, node, "node.read"); e != nil {
		return e
	}
	if typ == "version" {
		return requireCaps(ctx, tx, u, node, "config.read")
	}
	if typ == "operation" {
		if e = requireCaps(ctx, tx, u, node, "operation.read"); e != nil {
			return requireCaps(ctx, tx, u, node, "config.read", "stack.deploy")
		}
	}
	return nil
}

func (s *Store) mutateService(ctx context.Context, tx *sql.Tx, u serviceUser, r ServiceRequest, rt serviceRoute, b serviceBody, p *preparedService) (ServiceReply, string, string, error) {
	fail := func(e error) (ServiceReply, string, string, error) { return ServiceReply{}, "", "", e }
	now := time.Now().UnixMilli()
	if rt.kind == "users" || rt.kind == "user" || rt.kind == "nodes" || rt.kind == "grant" || rt.kind == "enrollment-tokens" || rt.kind == "revoke" {
		if e := requireAdmin(u); e != nil {
			return fail(e)
		}
	}
	switch {
	case rt.kind == "users" && r.Method == "POST":
		var n int
		if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE login_name=?`, b.Username).Scan(&n); e != nil {
			return fail(serviceStorageError(e))
		}
		if n != 0 {
			return fail(serviceError(409, "name_conflict"))
		}
		if !p.ready {
			return fail(errPrepareService)
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO users(id,enabled,auth_rev,login_name,password_hash,system_admin,created_at) VALUES(?,1,0,?,?,0,?)`, p.id, b.Username, p.hash, now); e != nil {
			return fail(serviceStorageError(e))
		}
		return serviceReply(201, map[string]any{"user_id": p.id}), "user", p.id, nil
	case rt.kind == "user" && r.Method == "PATCH":
		target, e := readServiceUser(ctx, tx, rt.user)
		if e != nil {
			return fail(e)
		}
		if e = checkETag(r.IfMatch, strconv.FormatUint(target.rev, 10)); e != nil {
			return fail(e)
		}
		if b.Enabled != nil && !*b.Enabled && target.admin && target.enabled {
			var n int
			if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE enabled=1 AND system_admin=1`).Scan(&n); e != nil {
				return fail(serviceStorageError(e))
			}
			if n <= 1 {
				return fail(serviceError(409, "last_admin"))
			}
		}
		if !p.ready {
			return fail(errPrepareService)
		}
		if b.Enabled != nil {
			target.enabled = *b.Enabled
			_, e = tx.ExecContext(ctx, `UPDATE users SET enabled=?,auth_rev=auth_rev+1 WHERE id=?`, target.enabled, rt.user)
		} else {
			_, e = tx.ExecContext(ctx, `UPDATE users SET password_hash=?,auth_rev=auth_rev+1 WHERE id=?`, p.hash, rt.user)
		}
		if e != nil {
			return fail(serviceStorageError(e))
		}
		if _, e = tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, rt.user); e != nil {
			return fail(serviceStorageError(e))
		}
		target.rev++
		reply := serviceReply(200, userBody(target))
		reply.ETag = `"` + strconv.FormatUint(target.rev, 10) + `"`
		return reply, "user", rt.user, nil
	case rt.kind == "nodes" && r.Method == "POST":
		if !p.ready {
			return fail(errPrepareService)
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO nodes(id,ledger_id,session_id,boot_id,frozen,display_name,created_at) VALUES(?,'','','',1,?,?)`, p.id, b.DisplayName, now); e != nil {
			return fail(serviceStorageError(e))
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO recovery_nodes(node_id,agent_boot,active_round) VALUES(?,'','')`, p.id); e != nil {
			return fail(serviceStorageError(e))
		}
		return serviceReply(201, map[string]any{"node_id": p.id, "status": "pending"}), "node", p.id, nil
	case rt.kind == "grant" && r.Method == "PUT":
		if e := requireCaps(ctx, tx, u, rt.node); e != nil {
			return fail(e)
		}
		target, e := readServiceUser(ctx, tx, rt.user)
		if e != nil {
			return fail(e)
		}
		if e = checkETag(r.IfMatch, strconv.FormatUint(target.rev, 10)); e != nil {
			return fail(e)
		}
		if !p.ready {
			return fail(errPrepareService)
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM grants WHERE user_id=? AND node_id=?`, rt.user, rt.node); e != nil {
			return fail(serviceStorageError(e))
		}
		for _, cap := range b.Capabilities {
			if _, e = tx.ExecContext(ctx, `INSERT INTO grants(user_id,node_id,capability) VALUES(?,?,?)`, rt.user, rt.node, cap); e != nil {
				return fail(serviceStorageError(e))
			}
		}
		if _, e = tx.ExecContext(ctx, `UPDATE users SET auth_rev=auth_rev+1 WHERE id=?`, rt.user); e != nil {
			return fail(serviceStorageError(e))
		}
		sort.Strings(b.Capabilities)
		reply := serviceReply(200, map[string]any{"user_id": rt.user, "capabilities": b.Capabilities, "auth_rev": target.rev + 1})
		reply.ETag = `"` + strconv.FormatUint(target.rev+1, 10) + `"`
		return reply, "grant", rt.user, nil
	case rt.kind == "enrollment-tokens" && r.Method == "POST":
		var generation int64
		var revoked sql.NullInt64
		e := tx.QueryRowContext(ctx, `SELECT credential_generation,revoked_at FROM nodes WHERE id=?`, rt.node).Scan(&generation, &revoked)
		if errors.Is(e, sql.ErrNoRows) {
			return fail(serviceError(404, "not_found"))
		}
		if e != nil {
			return fail(serviceStorageError(e))
		}
		if generation != 0 || revoked.Valid {
			return fail(serviceError(409, "node_already_enrolled"))
		}
		var active int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM enrollments WHERE node_id=? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>?`, rt.node, now).Scan(&active); e != nil {
			return fail(serviceStorageError(e))
		}
		if active > 0 && !b.ReplacePending {
			return fail(serviceError(409, "enrollment_active"))
		}
		if !p.ready {
			return fail(errPrepareService)
		}
		if _, e = tx.ExecContext(ctx, `UPDATE enrollments SET revoked_at=? WHERE node_id=? AND consumed_at IS NULL AND revoked_at IS NULL`, now, rt.node); e != nil {
			return fail(serviceStorageError(e))
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO enrollments(id,token_hash,node_id,expires_at,created_at) VALUES(?,?,?,?,?)`, p.id, secretHash(p.secret), rt.node, now+600000, now); e != nil {
			return fail(serviceStorageError(e))
		}
		return serviceReply(201, map[string]any{"enrollment_id": p.id, "status": "pending", "expires_at": timestamp(now + 600000), "token": p.secret}), "enrollment", p.id, nil
	case rt.kind == "revoke" && r.Method == "POST":
		if e := requireCaps(ctx, tx, u, rt.node); e != nil {
			return fail(e)
		}
		if !p.ready {
			return fail(errPrepareService)
		}
		for _, q := range []string{`UPDATE nodes SET revoked_at=?,session_id='',boot_id='',frozen=1 WHERE id=?`, `UPDATE enrollments SET revoked_at=? WHERE node_id=? AND revoked_at IS NULL`} {
			if _, e := tx.ExecContext(ctx, q, now, rt.node); e != nil {
				return fail(serviceStorageError(e))
			}
		}
		if _, e := tx.ExecContext(ctx, `UPDATE node_credentials SET enabled=0 WHERE node_id=?`, rt.node); e != nil {
			return fail(serviceStorageError(e))
		}
		return serviceReply(200, map[string]any{"node_id": rt.node, "status": "revoked"}), "node", rt.node, nil
	case (rt.kind == "stacks" || rt.kind == "versions") && r.Method == "POST":
		return s.mutateConfig(ctx, tx, u, r, rt, b, p)
	case rt.kind == "apply" && r.Method == "POST":
		return s.mutateApply(ctx, tx, u, r, rt, b, p)
	case rt.kind == "cancel" && r.Method == "POST":
		op, e := s.readOperation(ctx, tx, rt.operation)
		if errors.Is(e, ErrNotFound) {
			e = serviceError(404, "not_found")
		}
		if e != nil {
			return fail(e)
		}
		if e = requireCaps(ctx, tx, u, op.NodeID, "node.read", "operation.read", "operation.cancel"); e != nil {
			return fail(e)
		}
		if e = s.cancelTx(ctx, tx, rt.operation); e != nil {
			return fail(e)
		}
		op, e = s.readOperation(ctx, tx, rt.operation)
		if e != nil {
			return fail(e)
		}
		body, e := s.operationView(ctx, tx, op)
		if e != nil {
			return fail(e)
		}
		return serviceReply(200, body), "operation", rt.operation, nil
	}
	return fail(serviceError(404, "not_found"))
}
