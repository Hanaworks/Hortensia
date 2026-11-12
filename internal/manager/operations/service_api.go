package operations

import (
	"crypto/subtle"
	"encoding/json"
)

// Bootstrap is accepted only by exclusive creation of a new service database.
type Bootstrap struct{ Username, Password string }

// BrowserSession is a locator, never a cached grant or authorization decision.
type BrowserSession struct {
	store          *Store
	id, user, csrf string
	tokenHash      []byte
}

func (b *BrowserSession) CheckCSRF(value string) bool {
	return b != nil && b.csrf != "" && subtle.ConstantTimeCompare([]byte(b.csrf), []byte(value)) == 1
}

type ServiceReply struct {
	Status         int
	Body           json.RawMessage
	ETag, Location string
}
type BrowserLogin struct {
	Session *BrowserSession
	Token   string
	Reply   ServiceReply
}
type ServiceRequest struct {
	Method, Path, IdempotencyKey, IfMatch string
	Body                                  json.RawMessage
	Limit                                 int
	Cursor, NodeID, After                 string
}
type ServiceFieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type ServiceError struct {
	Status                               int
	Code, Category, Message, RetryAction string
	RetryAfter                           int
	FieldErrors                          []ServiceFieldError
}

func (e *ServiceError) Error() string { return e.Code }

type EnrollmentRequest struct {
	NodeID           string `json:"node_id"`
	LedgerID         string `json:"ledger_id"`
	CredentialSecret string `json:"credential_secret"`
}
type RotationRequest struct {
	LedgerID         string `json:"ledger_id"`
	CredentialSecret string `json:"credential_secret"`
}
type AuthenticatedNode struct {
	store        *Store
	node, ledger string
	generation   uint64
	hash         []byte
}
