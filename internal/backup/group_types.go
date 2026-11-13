package backup

import (
	"context"
	"errors"
)

type Component string

const (
	ComponentAgent   Component = "agent"
	ComponentManager Component = "manager"
)

type MaterialRole string

const (
	RoleIdentity   MaterialRole = "identity"
	RoleLedger     MaterialRole = "ledger"
	RoleJournal    MaterialRole = "journal"
	RoleDigest     MaterialRole = "digest"
	RoleEncryption MaterialRole = "encryption"
)

type MaterialSource struct {
	Role MaterialRole
	Path string
}
type BuildInfo struct {
	ApplicationVersion string  `json:"application_version"`
	BuildRevision      *string `json:"build_revision"`
	BuildDirty         *bool   `json:"build_dirty"`
}
type ExternalMaterial struct {
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Policy   string `json:"policy"`
}
type Source struct {
	Component         Component
	DatabaseFile      string
	MaterialsLockFile string
	Materials         []MaterialSource
	ConfigJSON        []byte
	Creator           BuildInfo
	ExternalMaterials []ExternalMaterial
}
type GroupRequest struct {
	Source              Source
	DataOut, SecretsOut string
}
type Material struct {
	Role MaterialRole
	Raw  []byte
}
type MaterialSet struct {
	Files      []Material
	ConfigJSON []byte
}
type IdentityRef struct {
	ManagerID string `json:"manager_id"`
	NodeID    string `json:"node_id"`
	LedgerID  string `json:"ledger_id"`
}
type KeyRef struct {
	Role  MaterialRole `json:"role"`
	KeyID string       `json:"key_id"`
}
type RowCount struct {
	Table string `json:"table"`
	Rows  uint64 `json:"rows"`
}
type DomainReport struct {
	Component                           Component
	Identity                            IdentityRef
	SchemaVersion, ProtocolVersion      int
	SQLiteVersion                       string
	Keys                                []KeyRef
	ReferencedEncryptionKeyIDs          []string
	TableRows, SealedRows               []RowCount
	SemanticFormat, SemanticFingerprint string
}
type DomainInspector func(context.Context, string, MaterialSet) (DomainReport, error)
type GroupState string

const (
	StatePartial              GroupState = "partial"
	StatePublicationUncertain GroupState = "publication_uncertain"
	StateComplete             GroupState = "complete"
)

type GroupResult struct {
	BackupID                     string
	DataManifest, SecretManifest string
	State                        GroupState
	Report                       DomainReport
}

var (
	ErrMaterialsBusy        = errors.New("backup materials busy")
	ErrMaterialsChanged     = errors.New("backup materials changed")
	ErrVerification         = errors.New("backup verification failed")
	ErrPublicationUncertain = errors.New("backup publication uncertain")
	ErrCleanup              = errors.New("backup cleanup failed")
)
