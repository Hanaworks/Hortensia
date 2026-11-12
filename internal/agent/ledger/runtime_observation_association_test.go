package ledger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRuntimeObservationAllowsUnassignedStackButRejectsFalseAssociation(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		name := "unassigned"
		if wrong {
			name = "wrong_stack"
		}
		t.Run(name, func(t *testing.T) {
			s, _, _, _ := runtimeStore(t)
			ctx := context.Background()
			r := runtimePrepare(t, s)
			runtimeStart(t, s, r, nil)
			if err := s.RecordProcessCreated(ctx, r.OperationID, 901); err != nil {
				t.Fatal(err)
			}
			e := runtimeResourceObservation(t, r, strings.Repeat("a", 64))
			var observation map[string]any
			if err := json.Unmarshal(e.Observation, &observation); err != nil {
				t.Fatal(err)
			}
			item := observation["items"].([]any)[0].(map[string]any)
			item["stack_id"] = nil
			if wrong {
				item["stack_id"] = "00000000-0000-4000-8000-000000000099"
			}
			e.Observation, _ = json.Marshal(observation)
			zero := 0
			event, err := s.RecordProcessOutcome(ctx, r.OperationID, ProcessOutcome{BootID: s.BootID(), Outcome: "normal_exit", ExitCode: &zero, FinishedAt: timestamp(time.Now().UnixMilli())}, e)
			if wrong {
				if err == nil {
					t.Fatal("false public association accepted")
				}
				return
			}
			if err != nil {
				t.Fatal("nullable public association blocked valid private ownership evidence", err)
			}
			var body map[string]any
			json.Unmarshal(event.Body, &body)
			if body["ownership_status"] != "consistent" {
				t.Fatal("null public association removed validated private ownership", body["ownership_status"])
			}
		})
	}
}
