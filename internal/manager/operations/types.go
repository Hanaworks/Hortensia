// Package operations is a controlled Manager coordination store. Fixture
// inputs are trusted test boundaries, not authentication or deployment APIs.
package operations

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var (
	ErrInput        = errors.New("manager input invalid")
	ErrIdentity     = errors.New("manager identity mismatch")
	ErrKey          = errors.New("manager key invalid")
	ErrSchema       = errors.New("manager schema unsupported")
	ErrStorage      = errors.New("manager storage failure")
	ErrNotFound     = errors.New("manager record not found")
	ErrPermission   = errors.New("manager permission denied")
	ErrFrozen       = errors.New("manager operation frozen")
	ErrConflict     = errors.New("manager fixed evidence conflict")
	ErrClaimed      = errors.New("manager resource claimed")
	ErrEvidence     = errors.New("manager evidence invalid")
	ErrKeyExhausted = errors.New("manager encryption key exhausted")
)

type Identity struct{ ManagerID, DigestKeyID, EncryptionKeyID string }
type Keys struct {
	DigestID, EncryptionID   string
	DigestKey, EncryptionKey []byte
}
type Binding struct{ NodeID, LedgerID, SessionID string }

type Operation struct {
	OperationID, UserID, NodeID, LedgerID, StackID, VersionID                string
	ProjectName, EndpointID, Digest, OwnerBoot, PreparedSession, State       string
	PermitID, PermitSession, StartNonce, ReleaseStatus, CreatedAt, UpdatedAt string
	PolicyRevision, Revision, AuthRev, LatestResultSeq, MaxEventSeq          uint64
	CancelRequested, Blocked                                                 bool
}
type Claim struct{ Kind, ResourceID string }
type Intent struct {
	Kind, State, TerminalState string
	Generation, ResultEventSeq uint64
}
type Event struct {
	OperationID, Digest, Kind string
	Seq                       uint64
	Body                      json.RawMessage
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func validID(value string) bool { return idPattern.MatchString(value) }
func validDigest(value string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == value
}
func projectResourceID(endpoint, project string) string {
	raw, _ := json.Marshal([]string{endpoint, project})
	return string(raw)
}
func timestamp(milliseconds int64) string {
	return time.UnixMilli(milliseconds).UTC().Format("2006-01-02T15:04:05.000Z")
}
