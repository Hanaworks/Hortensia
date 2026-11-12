package service

import (
	"encoding/json"
	"testing"
	"time"
)

func TestObservationSnapshotPagesAreFixedAndBound(t *testing.T) {
	now := time.Now()
	cache := newPageCache(func() time.Time { return now })
	q := observeRequest{QueryID: "q", OperationID: "op", StackID: "stack", Limit: 1}
	snap := querySnapshot{request: q, collectedAt: now.Format(time.RFC3339), items: []json.RawMessage{json.RawMessage(`{"id":"first"}`), json.RawMessage(`{"id":"second"}`)}}
	if e := cache.put(snap); e != nil {
		t.Fatal(e)
	}
	first, next, e := cache.page(q)
	if e != nil || len(first.items) != 1 || next == "" {
		t.Fatal("first page", e)
	}
	q.Cursor = next
	second, end, e := cache.page(q)
	if e != nil || len(second.items) != 1 || end != "" || string(second.items[0]) != `{"id":"second"}` {
		t.Fatal("second page", e)
	}
	q.Limit = 2
	if _, _, e = cache.page(q); e == nil {
		t.Fatal("changed limit accepted")
	}
	q.Limit = 1
	now = now.Add(5 * time.Minute)
	if _, _, e = cache.page(q); e == nil {
		t.Fatal("expired snapshot accepted")
	}
	fresh := newPageCache(time.Now)
	if _, _, e = fresh.page(q); e == nil {
		t.Fatal("cursor survived restart")
	}
}
