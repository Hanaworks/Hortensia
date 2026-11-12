package service

import (
	"context"
	"docker-manager/internal/agent/compose"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agent/workspace"
	"docker-manager/internal/protocol"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

type observeRequest struct {
	QueryID      string `json:"query_id"`
	OperationID  string `json:"operation_id"`
	StackID      string `json:"stack_id"`
	ResourceKind string `json:"resource_kind"`
	Cursor       string `json:"cursor"`
	Limit        int    `json:"limit"`
}
type querySnapshot struct {
	request     observeRequest
	created     time.Time
	collectedAt string
	items       []json.RawMessage
	failure     json.RawMessage
	cursors     map[string]int
	offsets     map[int]string
	bytes       int
}
type pageCache struct {
	mu      sync.Mutex
	now     func() time.Time
	queries map[string]querySnapshot
	bytes   int
}

var errCursor = errors.New("cursor_expired")
var errQuery = errors.New("query_conflict")
var errQueryBudget = errors.New("observation_budget_exceeded")

func newPageCache(now func() time.Time) *pageCache {
	return &pageCache{now: now, queries: map[string]querySnapshot{}}
}
func sameQuery(a, b observeRequest) bool {
	return a.QueryID == b.QueryID && a.OperationID == b.OperationID && a.StackID == b.StackID && a.Limit == b.Limit && a.ResourceKind == b.ResourceKind
}
func (c *pageCache) clean() {
	for id, s := range c.queries {
		if !c.now().Before(s.created.Add(5 * time.Minute)) {
			c.bytes -= s.bytes
			delete(c.queries, id)
		}
	}
}
func (c *pageCache) room() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clean()
	return len(c.queries) < 8 && c.bytes < 16<<20
}
func (c *pageCache) put(s querySnapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clean()
	if old, ok := c.queries[s.request.QueryID]; ok {
		if !sameQuery(old.request, s.request) {
			return errQuery
		}
		return nil
	}
	if s.created.IsZero() {
		s.created = c.now()
	}
	s.cursors = map[string]int{}
	s.offsets = map[int]string{}
	items := make([]json.RawMessage, len(s.items))
	for i, v := range s.items {
		items[i] = append(json.RawMessage{}, v...)
		s.bytes += len(v)
	}
	s.items = items
	s.failure = append(json.RawMessage{}, s.failure...)
	s.bytes += len(s.failure)
	if s.bytes > 8<<20 || len(c.queries) >= 8 || c.bytes+s.bytes > 16<<20 {
		return errQueryBudget
	}
	for offset := s.request.Limit; offset < len(items); offset += s.request.Limit {
		cursor, e := protocol.NewID()
		if e != nil {
			return e
		}
		s.cursors[cursor] = offset
		s.offsets[offset] = cursor
	}
	c.queries[s.request.QueryID] = s
	c.bytes += s.bytes
	return nil
}
func (c *pageCache) page(q observeRequest) (querySnapshot, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clean()
	s, ok := c.queries[q.QueryID]
	if !ok {
		return querySnapshot{}, "", errCursor
	}
	if !sameQuery(s.request, q) {
		return querySnapshot{}, "", errQuery
	}
	start := 0
	if q.Cursor != "" {
		var ok bool
		start, ok = s.cursors[q.Cursor]
		if !ok {
			return querySnapshot{}, "", errCursor
		}
	}
	end := start + q.Limit
	if end > len(s.items) {
		end = len(s.items)
	}
	s.items = append([]json.RawMessage{}, s.items[start:end]...)
	return s, s.offsets[end], nil
}
func (r *runtimeService) observationReply(ss *session, m incoming, q observeRequest, snap querySnapshot, next, code string) {
	var op any
	if q.OperationID != "" {
		op = q.OperationID
	}
	var cursor any
	if next != "" {
		cursor = next
	}
	var failure any
	if code != "" {
		failure = map[string]any{"category": "unavailable", "code": code, "message": "Resource observation is unavailable.", "retry_action": "query_operation"}
	} else if len(snap.failure) > 0 && string(snap.failure) != "null" {
		failure = snap.failure
	}
	at := snap.collectedAt
	if at == "" {
		at = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	items := snap.items
	if items == nil {
		items = []json.RawMessage{}
	}
	raw, e := wireMessage("observation", ss.id, m.env.MessageID, "", map[string]any{"query_id": q.QueryID, "operation_id": op, "node_id": r.s.Identity().NodeID, "collected_at": at, "items": items, "next_cursor": cursor, "error": failure})
	if e == nil {
		_ = ss.send(raw)
	}
}
func (r *runtimeService) observe(ss *session, m incoming) {
	var q observeRequest
	if json.Unmarshal(m.env.Body, &q) != nil {
		return
	}
	if !r.readable {
		r.observationReply(ss, m, q, querySnapshot{}, "", "execution_profile_unavailable")
		return
	}
	if q.OperationID == "" {
		r.observationReply(ss, m, q, querySnapshot{}, "", "observation_unsupported")
		return
	}
	if snap, next, e := r.pages.page(q); e == nil {
		r.observationReply(ss, m, q, snap, next, "")
		return
	} else if errors.Is(e, errQuery) {
		r.observationReply(ss, m, q, querySnapshot{}, "", "query_conflict")
		return
	}
	if q.Cursor != "" {
		r.observationReply(ss, m, q, querySnapshot{}, "", "cursor_expired")
		return
	}
	if !r.pages.room() {
		r.observationReply(ss, m, q, querySnapshot{}, "", "observation_budget_exceeded")
		return
	}
	record, e := r.s.Get(r.ctx, q.OperationID)
	if e != nil || q.StackID != "" && q.StackID != record.StackID {
		r.observationReply(ss, m, q, querySnapshot{}, "", "query_conflict")
		return
	}
	if e = r.recoverStoredOutcome(q.OperationID); e != nil {
		r.observationReply(ss, m, q, querySnapshot{}, "", "observation_unavailable")
		return
	}
	query, e := r.s.BeginObservationQuery(r.ctx, q.OperationID, q.QueryID, q.Limit)
	if e != nil {
		code := "observation_unsupported"
		if errors.Is(e, ledger.ErrScanExpired) {
			code = "cursor_expired"
		}
		r.observationReply(ss, m, q, querySnapshot{}, "", code)
		return
	}
	if query.Disposition != "collect" {
		r.observationReply(ss, m, q, querySnapshot{}, "", "cursor_expired")
		return
	}
	p, e := r.readPrepared(r.ctx, record)
	if e != nil {
		r.observationReply(ss, m, q, querySnapshot{}, "", "resource_query_failed")
		return
	}
	evidence, _ := r.engine.Observe(r.ctx, p)
	if _, e = r.s.RecordObservationUpdate(r.ctx, q.OperationID, q.QueryID, evidence); e != nil {
		r.observationReply(ss, m, q, querySnapshot{}, "", "observation_unavailable")
		return
	}
	var observation struct {
		CollectedAt string            `json:"collected_at"`
		Items       []json.RawMessage `json:"items"`
		Error       json.RawMessage   `json:"error"`
	}
	if json.Unmarshal(evidence.Observation, &observation) != nil {
		r.observationReply(ss, m, q, querySnapshot{}, "", "resource_query_failed")
		return
	}
	snap := querySnapshot{request: q, created: query.CreatedAt, collectedAt: observation.CollectedAt, items: observation.Items, failure: observation.Error}
	if e = r.pages.put(snap); e != nil {
		r.observationReply(ss, m, q, querySnapshot{}, "", "observation_budget_exceeded")
		return
	}
	snap, next, e := r.pages.page(q)
	if e != nil {
		r.observationReply(ss, m, q, querySnapshot{}, "", "cursor_expired")
		return
	}
	r.observationReply(ss, m, q, snap, next, "")
}
func (r *runtimeService) readPrepared(ctx context.Context, record ledger.Record) (compose.Prepared, error) {
	r.mu.Lock()
	o := r.ops[record.OperationID]
	var saved compose.Prepared
	if o != nil {
		saved = o.prepared
	}
	r.mu.Unlock()
	if saved.Package.OperationID != "" {
		return saved, nil
	}
	evidence, e := r.s.ReadPreparationEvidence(ctx, record.OperationID)
	if e != nil {
		return compose.Prepared{}, e
	}
	raw, e := r.s.ReadExecutionInput(ctx, record.OperationID)
	if e != nil {
		return compose.Prepared{}, e
	}
	defer clear(raw)
	p, e := workspace.Materialize(ctx, r.c.StateDir, workspace.Input{OperationID: record.OperationID, InputFingerprint: record.LocalFingerprint, Payload: raw})
	if e != nil {
		return compose.Prepared{}, e
	}
	if p.ManifestFingerprint != evidence.ManifestFingerprint || p.WorkspaceID != evidence.WorkspaceID {
		return compose.Prepared{}, ErrRuntime
	}
	return compose.Prepared{Package: p, Evidence: evidence}, nil
}
