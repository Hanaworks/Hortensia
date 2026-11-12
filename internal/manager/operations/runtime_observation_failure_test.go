package operations

import (
	"context"
	"testing"
	"time"
)

func TestRuntimeObservationStopSurvivesDelayedTransportFailures(t *testing.T) {
	for _, terminal := range []string{"cursor_expired", "observation_failed"} {
		t.Run(terminal, func(t *testing.T) {
			p, login, auth, _ := runtimePair(t)
			ctx := context.Background()
			op := runtimeAuthorize(t, p, auth, runtimeApply(t, p, login))
			eventReceive(t, p.m, p.b, eventResult(t, p.m, p.b, op, "normal_exit", 0, "error", "unknown", ""))
			now := time.Now()
			d, err := p.m.NextNodeDelivery(ctx, p.b, now)
			if err != nil || d == nil || d.Kind != "observe" {
				t.Fatal(d, err)
			}
			if err := p.m.RecordDeliveryFailure(ctx, p.b, d.MessageID, terminal, now); err != nil {
				t.Fatal(err)
			}
			for _, late := range []string{"write_failed", "timeout", "queue_full", "connection_closed", terminal} {
				if err := p.m.RecordDeliveryFailure(ctx, p.b, d.MessageID, late, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if next, err := p.m.NextNodeDelivery(ctx, p.b, now.Add(time.Hour)); err != nil || next != nil {
					t.Fatalf("late %s revived stopped %s query: %v %v", late, terminal, next != nil, err)
				}
				var stored string
				if err := p.m.db.QueryRow(`SELECT failure_code FROM runtime_deliveries WHERE message_id=?`, d.MessageID).Scan(&stored); err != nil || stored != terminal {
					t.Fatal("stop fact was overwritten", stored, err)
				}
			}
		})
	}
}
