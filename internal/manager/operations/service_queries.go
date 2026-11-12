package operations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Store) nodeView(ctx context.Context, q querier, u serviceUser, id string) (map[string]any, error) {
	var name, ledger, session, boot, caps string
	var generation uint64
	var frozen bool
	var revoked, last sql.NullInt64
	var profile sql.NullString
	e := q.QueryRowContext(ctx, `SELECT display_name,ledger_id,session_id,boot_id,credential_generation,frozen,revoked_at,last_seen_at,agent_capabilities_json,execution_profile_json FROM nodes WHERE id=?`, id).Scan(&name, &ledger, &session, &boot, &generation, &frozen, &revoked, &last, &caps, &profile)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, serviceError(404, "not_found")
	}
	if e != nil {
		return nil, serviceStorageError(e)
	}
	permissionErr := requireCaps(ctx, q, u, id, "node.read")
	if permissionErr != nil {
		var se *ServiceError
		if !errors.As(permissionErr, &se) {
			return nil, permissionErr
		}
	}
	visible := permissionErr == nil
	if !visible && !u.admin {
		return nil, serviceError(404, "not_found")
	}
	status := "offline"
	switch {
	case revoked.Valid:
		status = "revoked"
	case generation == 0:
		status = "pending"
	case frozen:
		status = "reconciling"
	case session == "" || boot != s.boot:
		status = "offline"
	default:
		status = "online"
	}
	var seen any
	if last.Valid {
		seen = timestamp(last.Int64)
	}
	grants := []string{}
	agent := []string{}
	var ep any
	if visible {
		rows, e := q.QueryContext(ctx, `SELECT capability FROM grants WHERE node_id=? AND user_id=? ORDER BY capability`, id, u.id)
		if e != nil {
			return nil, serviceStorageError(e)
		}
		for rows.Next() {
			var c string
			if e = rows.Scan(&c); e != nil {
				rows.Close()
				return nil, serviceStorageError(e)
			}
			grants = append(grants, c)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, serviceStorageError(e)
		}
		if json.Unmarshal([]byte(caps), &agent) != nil {
			return nil, ErrEvidence
		}
		if profile.Valid {
			ep = json.RawMessage(profile.String)
		}
	}
	return map[string]any{"node_id": id, "display_name": name, "status": status, "capabilities": grants, "agent_capabilities": agent, "last_seen_at": seen, "execution_profile": ep, "observation": nil}, nil
}
func (s *Store) operationView(ctx context.Context, q querier, op Operation) (map[string]any, error) {
	if e := s.requireOperationEvidence(ctx, q, op); e != nil {
		return nil, e
	}
	var result any
	if op.LatestResultSeq > 0 {
		event, e := s.readEvent(ctx, q, op, op.LatestResultSeq)
		if e != nil {
			return nil, e
		}
		defer clear(event.Body)
		var m map[string]json.RawMessage
		if json.Unmarshal(event.Body, &m) != nil {
			return nil, ErrEvidence
		}
		out := map[string]json.RawMessage{}
		for _, key := range []string{"outcome", "ownership_status", "interrupt_reason", "exit_code", "error", "observation", "not_started", "output_truncated", "finished_at"} {
			value, ok := m[key]
			if !ok {
				return nil, ErrEvidence
			}
			out[key] = value
		}
		result = out
	}
	return map[string]any{"operation_id": op.OperationID, "node_id": op.NodeID, "stack_id": op.StackID, "version_id": op.VersionID, "operation_type": "stack.apply", "revision": op.Revision, "state": op.State, "cancel_requested": op.CancelRequested, "created_at": op.CreatedAt, "updated_at": op.UpdatedAt, "result": result, "release_status": op.ReleaseStatus}, nil
}
func (s *Store) queryService(ctx context.Context, tx *sql.Tx, u serviceUser, r ServiceRequest, rt serviceRoute) (ServiceReply, error) {
	switch rt.kind {
	case "users", "nodes", "grants", "stacks", "operations":
		return s.listService(ctx, tx, u, r, rt)
	case "user":
		if e := requireAdmin(u); e != nil {
			return ServiceReply{}, e
		}
		target, e := readServiceUser(ctx, tx, rt.user)
		if e != nil {
			return ServiceReply{}, e
		}
		reply := serviceReply(200, userBody(target))
		reply.ETag = `"` + strconv.FormatUint(target.rev, 10) + `"`
		return reply, nil
	case "node":
		view, e := s.nodeView(ctx, tx, u, rt.node)
		return serviceReply(200, view), e
	case "stack", "version":
		st, e := readServiceStack(ctx, tx, rt.stack)
		if e != nil {
			return ServiceReply{}, e
		}
		caps := []string{"node.read"}
		if rt.kind == "version" {
			caps = append(caps, "config.read")
		}
		if e = requireCaps(ctx, tx, u, st.node, caps...); e != nil {
			return ServiceReply{}, e
		}
		if rt.kind == "stack" {
			view, e := s.stackView(ctx, tx, st)
			if e != nil {
				return ServiceReply{}, e
			}
			reply := serviceReply(200, view)
			reply.ETag = `"` + st.current + `"`
			return reply, nil
		}
		raw, e := s.readConfig(ctx, tx, st, rt.version)
		if e != nil {
			return ServiceReply{}, e
		}
		defer clear(raw)
		reply := serviceReply(200, map[string]any{"version_id": rt.version, "config": json.RawMessage(raw)})
		reply.ETag = `"` + rt.version + `"`
		return reply, nil
	case "operation", "output":
		op, e := s.readOperation(ctx, tx, rt.operation)
		if errors.Is(e, ErrNotFound) {
			e = serviceError(404, "not_found")
		}
		if e != nil {
			return ServiceReply{}, e
		}
		caps := []string{"node.read", "operation.read"}
		if rt.kind == "output" {
			caps = append(caps, "operation.output.read")
		}
		if e = requireCaps(ctx, tx, u, op.NodeID, caps...); e != nil {
			return ServiceReply{}, e
		}
		if rt.kind == "output" {
			return s.outputView(ctx, tx, op, r)
		}
		v, e := s.operationView(ctx, tx, op)
		return serviceReply(200, v), e
	case "submission":
		var typ, id, path string
		var status int
		e := tx.QueryRowContext(ctx, `SELECT result_type,result_id,path,response_status FROM idempotency_records WHERE user_id=? AND idempotency_key=?`, u.id, rt.key).Scan(&typ, &id, &path, &status)
		if errors.Is(e, sql.ErrNoRows) {
			return ServiceReply{}, serviceError(404, "not_found")
		}
		if e != nil {
			return ServiceReply{}, serviceStorageError(e)
		}
		if e = s.receiptPermission(ctx, tx, u, typ, id, path); e != nil {
			return ServiceReply{}, e
		}
		return serviceReply(200, map[string]any{"result_type": typ, "result_id": id, "response_status": status}), nil
	}
	return ServiceReply{}, serviceError(404, "not_found")
}

type serviceCursor struct {
	Path, Node, User string
	Revision         uint64
	Time             int64
	ID               string
	Expires          int64
}

func (s *Store) cursorMAC(raw []byte) ([]byte, error) {
	keys, e := s.keyCopies()
	if e != nil {
		return nil, e
	}
	defer clear(keys.DigestKey)
	defer clear(keys.EncryptionKey)
	h := hmac.New(sha256.New, keys.DigestKey)
	h.Write([]byte("dm-http-cursor-v1\x00"))
	h.Write(raw)
	return h.Sum(nil), nil
}
func (s *Store) cursorRead(raw string, want serviceCursor) (serviceCursor, error) {
	if raw == "" {
		return want, nil
	}
	if len(raw) > 1024 {
		return want, serviceError(400, "invalid_field")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return want, serviceError(400, "invalid_field")
	}
	data, e := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if e != nil {
		return want, serviceError(400, "invalid_field")
	}
	sig, e := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if e != nil {
		return want, serviceError(400, "invalid_field")
	}
	mac, e := s.cursorMAC(data)
	if e != nil {
		return want, e
	}
	if !hmac.Equal(sig, mac) {
		return want, serviceError(400, "invalid_field")
	}
	var c serviceCursor
	if json.Unmarshal(data, &c) != nil || c.Path != want.Path || c.Node != want.Node || c.User != want.User || !validID(c.ID) {
		return want, serviceError(400, "invalid_field")
	}
	if c.Revision != want.Revision {
		return want, serviceError(403, "permission_denied")
	}
	if time.Now().UnixMilli() >= c.Expires {
		return want, serviceError(410, "cursor_expired")
	}
	return c, nil
}
func (s *Store) cursorWrite(c serviceCursor) (string, error) {
	c.Expires = time.Now().Add(15 * time.Minute).UnixMilli()
	raw, _ := json.Marshal(c)
	mac, e := s.cursorMAC(raw)
	if e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}
func (s *Store) listService(ctx context.Context, tx *sql.Tx, u serviceUser, r ServiceRequest, rt serviceRoute) (ServiceReply, error) {
	limit := r.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return ServiceReply{}, serviceError(400, "invalid_field")
	}
	table, idCol, filter := "", "id", "1=1"
	args := []any{}
	switch rt.kind {
	case "users", "grants":
		if e := requireAdmin(u); e != nil {
			return ServiceReply{}, e
		}
		table = "users"
		if rt.kind == "grants" {
			if e := requireCaps(ctx, tx, u, rt.node); e != nil {
				return ServiceReply{}, e
			}
		}
	case "nodes":
		table = "nodes"
		if !u.admin {
			filter = `EXISTS(SELECT 1 FROM grants g WHERE g.node_id=nodes.id AND g.user_id=? AND g.capability='node.read')`
			args = append(args, u.id)
		}
	case "stacks":
		if e := requireCaps(ctx, tx, u, rt.node, "node.read"); e != nil {
			return ServiceReply{}, e
		}
		table = "stacks"
		filter = "node_id=?"
		args = append(args, rt.node)
	case "operations":
		if e := requireCaps(ctx, tx, u, rt.node, "node.read", "operation.read"); e != nil {
			return ServiceReply{}, e
		}
		table = "operations"
		idCol = "operation_id"
		filter = "node_id=?"
		args = append(args, rt.node)
	}
	cursor, e := s.cursorRead(r.Cursor, serviceCursor{Path: r.Path, Node: rt.node, User: u.id, Revision: u.rev})
	if e != nil {
		return ServiceReply{}, e
	}
	args = append(args, cursor.Time, cursor.Time, cursor.ID, limit+1)
	rows, e := tx.QueryContext(ctx, `SELECT `+idCol+`,created_at FROM `+table+` WHERE (`+filter+`) AND (created_at>? OR (created_at=? AND `+idCol+`>?)) ORDER BY created_at,`+idCol+` LIMIT ?`, args...)
	if e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	type row struct {
		id      string
		created int64
	}
	selected := []row{}
	for rows.Next() {
		var x row
		if e = rows.Scan(&x.id, &x.created); e != nil {
			rows.Close()
			return ServiceReply{}, serviceStorageError(e)
		}
		selected = append(selected, x)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	more := len(selected) > limit
	if more {
		selected = selected[:limit]
	}
	items := []any{}
	for _, x := range selected {
		switch rt.kind {
		case "users":
			v, e := readServiceUser(ctx, tx, x.id)
			if e != nil {
				return ServiceReply{}, e
			}
			items = append(items, userBody(v))
		case "nodes":
			v, e := s.nodeView(ctx, tx, u, x.id)
			if e != nil {
				return ServiceReply{}, e
			}
			items = append(items, v)
		case "stacks":
			v, e := readServiceStack(ctx, tx, x.id)
			if e != nil {
				return ServiceReply{}, e
			}
			view, e := s.stackView(ctx, tx, v)
			if e != nil {
				return ServiceReply{}, e
			}
			items = append(items, view)
		case "operations":
			v, e := s.readOperation(ctx, tx, x.id)
			if e != nil {
				return ServiceReply{}, e
			}
			body, e := s.operationView(ctx, tx, v)
			if e != nil {
				return ServiceReply{}, e
			}
			items = append(items, body)
		case "grants":
			v, e := readServiceUser(ctx, tx, x.id)
			if e != nil {
				return ServiceReply{}, e
			}
			caps := []string{}
			rs, e := tx.QueryContext(ctx, `SELECT capability FROM grants WHERE node_id=? AND user_id=? ORDER BY capability`, rt.node, x.id)
			if e != nil {
				return ServiceReply{}, serviceStorageError(e)
			}
			for rs.Next() {
				var c string
				if e = rs.Scan(&c); e != nil {
					rs.Close()
					return ServiceReply{}, serviceStorageError(e)
				}
				caps = append(caps, c)
			}
			e = rs.Err()
			rs.Close()
			if e != nil {
				return ServiceReply{}, serviceStorageError(e)
			}
			items = append(items, map[string]any{"user_id": x.id, "capabilities": caps, "auth_rev": v.rev})
		}
	}
	var next any
	if more {
		last := selected[len(selected)-1]
		cursor.Time = last.created
		cursor.ID = last.id
		next, e = s.cursorWrite(cursor)
		if e != nil {
			return ServiceReply{}, e
		}
	}
	return serviceReply(200, map[string]any{"items": items, "next_cursor": next}), nil
}
func (s *Store) outputView(ctx context.Context, q querier, op Operation, r ServiceRequest) (ServiceReply, error) {
	if e := s.requireOperationEvidence(ctx, q, op); e != nil {
		return ServiceReply{}, e
	}
	raw := []byte{}
	truncated := false
	if op.LatestResultSeq > 0 {
		event, e := s.readEvent(ctx, q, op, op.LatestResultSeq)
		if e != nil {
			return ServiceReply{}, e
		}
		defer clear(event.Body)
		var v struct {
			Output    string `json:"output_b64"`
			Truncated bool   `json:"output_truncated"`
		}
		if json.Unmarshal(event.Body, &v) != nil {
			return ServiceReply{}, ErrEvidence
		}
		raw, e = base64.StdEncoding.Strict().DecodeString(v.Output)
		if e != nil || !utf8.Valid(raw) || len(raw) > 1<<20 {
			return ServiceReply{}, ErrEvidence
		}
		truncated = v.Truncated
	}
	after := r.After
	if after == "" {
		after = "0"
	}
	for _, c := range after {
		if c < '0' || c > '9' {
			return ServiceReply{}, serviceError(400, "invalid_field")
		}
	}
	start, e := strconv.ParseUint(after, 10, 32)
	if e != nil || int(start) > len(raw) || (int(start) < len(raw) && !utf8.RuneStart(raw[start])) {
		return ServiceReply{}, serviceError(400, "invalid_field")
	}
	limit := r.Limit
	if limit == 0 {
		limit = 65536
	}
	if limit < 1 || limit > 262144 {
		return ServiceReply{}, serviceError(400, "invalid_field")
	}
	end := int(start) + limit
	if end > len(raw) {
		end = len(raw)
	}
	for end > int(start) && end < len(raw) && !utf8.RuneStart(raw[end]) {
		end--
	}
	if end == int(start) && end < len(raw) {
		return ServiceReply{}, serviceError(400, "invalid_field")
	}
	return serviceReply(200, map[string]any{"text": string(raw[start:end]), "next_cursor": strconv.Itoa(end), "truncated": truncated}), nil
}
