package ledger

import (
	"encoding/json"
	"time"
)

const PhasePreparing = "preparing"

// RuntimeOptions fixes the execution profile admitted by this process. An empty
// EngineID permits recovery/read-only work, but never preparation or execution.
type RuntimeOptions struct {
	Now     func() time.Time
	Profile RuntimeProfile
}
type RuntimeProfile struct {
	EndpointID     string
	PolicyRevision uint64
	Fingerprint    string
	EngineID       string
	DockerVersion  string
	ComposeVersion string
}

type PrepareReceipt struct {
	Record      Record
	Disposition string
}
type EventRef struct {
	OperationID, Kind string
	Seq               uint64
}

// ResourceRecord is private ownership evidence, never a Docker inspect dump.
// LogicalKey is service/number for containers, or the normalized resource key.
// Volume ResourceRef is its name+driver+creation fingerprint (weaker than ID).
type ResourceRecord struct {
	Kind            string `json:"kind"`
	ResourceRef     string `json:"resource_ref"`
	LogicalKey      string `json:"logical_key"`
	Classification  string `json:"classification"`
	CreatedAt       string `json:"created_at"`
	ProjectName     string `json:"project_name"`
	Service         string `json:"service"`
	ContainerNumber int    `json:"container_number"`
	Driver          string `json:"driver"`
}
type Baseline struct {
	CollectedAt string           `json:"collected_at"`
	Resources   []ResourceRecord `json:"resources"`
}
type Replacement struct{ Kind, LogicalKey, OldRef, NewRef string }

// ExpectedResource records a normalized Compose logical resource. External
// dependencies are validated by the adapter and excluded from managed ownership.
type ExpectedResource struct {
	Kind, LogicalKey string
	External         bool
}
type PreparationEvidence struct {
	OperationID, InputFingerprint                               string
	WorkspaceID, ManifestFingerprint                            string
	ProfileFingerprint, EngineID, DockerVersion, ComposeVersion string
	OwnershipRevision                                           uint64
	Pre                                                         Baseline
	Expected                                                    []ExpectedResource
}
type Failure struct {
	Code, Message string
	session       string
	Retryable     bool
	// BootID must identify the live Store and preparation must have no start fact.
	BootID string
}
type ProcessOutcome struct {
	BootID          string
	Outcome         string
	ExitCode        *int
	InterruptReason string
	FinishedAt      string
	Output          []byte
	OutputTruncated bool
	ErrorCode       string
}
type ObservationEvidence struct {
	// Observation is the exact shared protocol Observation object; validated
	// before persistence. Private fields below must agree with its collection.
	Observation  json.RawMessage
	Post         Baseline
	Replacements []Replacement
	// Conflicts are concrete resource identities discovered by the collector,
	// not an assertion that a caller trusts or distrusts the overall evidence.
	Conflicts []ResourceRecord
}
type CancelDecision struct {
	Disposition string
	Event       *Event
}
type ObservationQuery struct {
	QueryID, OperationID, BootID string
	Limit                        int
	CreatedAt                    time.Time
	ResultSeq                    uint64
	Disposition                  string
}
type ProjectBinding struct {
	EndpointID, ProjectName, StackID, Status, LastOperationID string
	Revision                                                  uint64
	Baseline                                                  Baseline
}
