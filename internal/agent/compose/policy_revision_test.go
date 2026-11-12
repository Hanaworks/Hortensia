package compose

import (
	"context"
	"encoding/json"
	"testing"

	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agent/workspace"
)

func TestObservationAcceptsMatchingInitialPolicyRevisionZero(t *testing.T) {
	p, e, _ := testPackage(t, "services: {app: {image: alpine}}", nil, "")
	payload := p.Payload
	payload.ExecutionProfile.PolicyRevision = 0
	raw, _ := json.Marshal(payload)
	fresh, err := workspace.Materialize(context.Background(), t.TempDir(), workspace.Input{OperationID: p.OperationID, InputFingerprint: p.InputFingerprint, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	e.profile.Runtime.PolicyRevision = 0
	observed, err := e.Observe(context.Background(), observationInput(fresh, e, ledger.PreparationEvidence{}))
	var view observation
	json.Unmarshal(observed.Observation, &view)
	if err != nil || view.Status != "complete" {
		t.Fatalf("matching schema-valid policy revision zero rejected: %v %s", err, observed.Observation)
	}
}
