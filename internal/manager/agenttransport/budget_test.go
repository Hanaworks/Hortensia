package agenttransport

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"docker-manager/internal/agentwire"
	"docker-manager/internal/protocol"
)

func TestControlRetryBudgetRejectsBeforeQueueingNewID(t *testing.T) {
	c := &connection{ctx: context.Background(), queue: make(chan outbound, 32), controls: map[string]retryState{}}
	for i := 0; i < 65536; i++ {
		c.controls[strconv.Itoa(i)] = retryState{next: time.Now().Add(time.Hour)}
	}
	raw, err := protocol.NewMessage("reconcile", id(t), "", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.queueControl(raw); !errors.Is(err, agentwire.ErrBudget) {
		t.Fatalf("new control over budget: %v", err)
	}
	if len(c.controls) != 65536 || len(c.queue) != 0 {
		t.Fatal("budget rejection mutated retry state or queued request")
	}
}
