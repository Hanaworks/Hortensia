package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"docker-manager/internal/startgate"
)

// Record contains metadata only. LocalFingerprint is private Agent evidence,
// never a public protocol field. Times are UTC RFC3339 with milliseconds.
type Record struct {
	Identity
	OperationID, StackID, VersionID string
	Digest, LocalFingerprint        string
	ProjectName, EndpointID         string
	PolicyRevision                  uint64
	PreparedSession, BootID, Phase  string
	CreatedAt, StartedAt            string
	Permit                          *startgate.Permit
}

type Claim struct{ Kind, ResourceID string }

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func projectResourceID(endpoint, project string) string {
	raw, _ := json.Marshal([]string{endpoint, project})
	return string(raw)
}

func timestamp(millis int64) string {
	return time.UnixMilli(millis).UTC().Format("2006-01-02T15:04:05.000Z")
}

func (s *Store) readRecord(ctx context.Context, q querier, operation string) (Record, error) {
	r := Record{Identity: s.identity}
	var digest, fingerprint []byte
	var created int64
	var started, revision sql.NullInt64
	var session, nonce, permit, reply sql.NullString
	err := q.QueryRowContext(ctx, `SELECT operation_id,stack_id,version_id,input_digest,local_fingerprint,project_name,docker_endpoint_id,policy_revision,prepared_session,owner_boot_id,phase,session_id,start_nonce,permit_id,auth_rev,reply_to,created_at,started_at FROM executions WHERE operation_id=?`, operation).Scan(&r.OperationID, &r.StackID, &r.VersionID, &digest, &fingerprint, &r.ProjectName, &r.EndpointID, &r.PolicyRevision, &r.PreparedSession, &r.BootID, &r.Phase, &session, &nonce, &permit, &revision, &reply, &created, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, ErrStorage
	}
	r.Digest = base64.RawURLEncoding.EncodeToString(digest)
	r.LocalFingerprint = base64.RawURLEncoding.EncodeToString(fingerprint)
	r.CreatedAt = timestamp(created)
	if r.Phase == PhaseStartCommitted {
		if !session.Valid || !nonce.Valid || !permit.Valid || !revision.Valid || !reply.Valid || !started.Valid {
			return Record{}, ErrStorage
		}
		r.StartedAt = timestamp(started.Int64)
		r.Permit = &startgate.Permit{Session: session.String, Operation: r.OperationID, Digest: r.Digest, Nonce: nonce.String, PermitID: permit.String, AuthRev: uint64(revision.Int64), ReplyTo: reply.String}
	}
	return r, nil
}

func readClaims(ctx context.Context, q querier, operation string) ([]Claim, error) {
	rows, err := q.QueryContext(ctx, `SELECT resource_kind,resource_id FROM local_claims WHERE operation_id=? ORDER BY resource_kind,resource_id`, operation)
	if err != nil {
		return nil, ErrStorage
	}
	defer rows.Close()
	claims := make([]Claim, 0, 2)
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.Kind, &c.ResourceID); err != nil {
			return nil, ErrStorage
		}
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrStorage
	}
	return claims, nil
}

func (s *Store) Get(ctx context.Context, operationID string) (Record, error) {
	return s.readRecord(ctx, s.db, operationID)
}
func (s *Store) Claims(ctx context.Context, operationID string) ([]Claim, error) {
	return readClaims(ctx, s.db, operationID)
}

// RecordReadyFixture validates an existing prepare wire message and atomically
// records its stable input and claims. It performs no materialization, preflight,
// or ready notification, and must not be used as a real deployment handler.
func (s *Store) RecordReadyFixture(ctx context.Context, rawPrepare []byte) (Record, error) {
	if s.runtime != nil {
		return Record{}, ErrSchema
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return Record{}, ErrStorage
	}
	key := append([]byte(nil), s.key...)
	s.mu.Unlock()
	defer clear(key)
	env, err := s.validator.Validate(rawPrepare)
	if err != nil || env.Type != "prepare" || env.SessionID == nil {
		return Record{}, ErrInput
	}
	var body struct {
		Operation string `json:"operation_id"`
		Digest    string `json:"input_digest"`
		Node      string `json:"node_id"`
		Stack     string `json:"stack_id"`
		Version   string `json:"version_id"`
		Payload   string `json:"payload_b64"`
	}
	if err := json.Unmarshal(env.Body, &body); err != nil {
		return Record{}, ErrInput
	}
	if body.Node != s.identity.NodeID {
		return Record{}, ErrIdentity
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(body.Payload)
	if err != nil {
		return Record{}, ErrInput
	}
	var input struct {
		Project string `json:"project_name"`
		Profile struct {
			Endpoint string      `json:"docker_endpoint_id"`
			Revision json.Number `json:"policy_revision"`
		} `json:"execution_profile"`
	}
	if err := json.Unmarshal(payload, &input); err != nil {
		return Record{}, ErrInput
	}
	// Validator has already checked exact integral value and the safe-integer
	// range; float conversion here accepts legal 1.0 and 1e0 representations.
	revision, err := input.Profile.Revision.Float64()
	if err != nil {
		return Record{}, ErrInput
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	fingerprint := mac.Sum(nil)
	digest, err := base64.RawURLEncoding.Strict().DecodeString(body.Digest)
	if err != nil {
		return Record{}, ErrInput
	}
	r := Record{Identity: s.identity, OperationID: body.Operation, StackID: body.Stack, VersionID: body.Version, Digest: body.Digest, LocalFingerprint: base64.RawURLEncoding.EncodeToString(fingerprint), ProjectName: input.Project, EndpointID: input.Profile.Endpoint, PolicyRevision: uint64(revision), PreparedSession: *env.SessionID, BootID: s.boot, Phase: PhaseReady}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, ErrStorage
	}
	defer tx.Rollback()
	if err := s.requireRecoveryMutation(ctx, tx, *env.SessionID); err != nil {
		return Record{}, err
	}
	existing, err := s.readRecord(ctx, tx, r.OperationID)
	if err == nil {
		if existing.BootID != s.boot {
			return Record{}, ErrFrozen
		}
		if existing.Identity != r.Identity || existing.StackID != r.StackID || existing.VersionID != r.VersionID || existing.Digest != r.Digest || existing.LocalFingerprint != r.LocalFingerprint || existing.ProjectName != r.ProjectName || existing.EndpointID != r.EndpointID || existing.PolicyRevision != r.PolicyRevision || existing.PreparedSession != r.PreparedSession {
			return Record{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Record{}, err
	}
	created := time.Now().UnixMilli()
	r.CreatedAt = timestamp(created)
	if _, err := tx.ExecContext(ctx, `INSERT INTO executions(operation_id,stack_id,version_id,input_digest,local_fingerprint,project_name,docker_endpoint_id,policy_revision,prepared_session,owner_boot_id,phase,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.OperationID, r.StackID, r.VersionID, digest, fingerprint, r.ProjectName, r.EndpointID, r.PolicyRevision, r.PreparedSession, s.boot, PhaseReady, created); err != nil {
		return Record{}, ErrStorage
	}
	for _, claim := range []Claim{{"stack", r.StackID}, {"project", projectResourceID(r.EndpointID, r.ProjectName)}} {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT operation_id FROM local_claims WHERE resource_kind=? AND resource_id=?`, claim.Kind, claim.ResourceID).Scan(&owner)
		if err == nil {
			return Record{}, ErrClaimed
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrStorage
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_claims(resource_kind,resource_id,operation_id) VALUES(?,?,?)`, claim.Kind, claim.ResourceID, r.OperationID); err != nil {
			return Record{}, ErrStorage
		}
	}
	if s.journal != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_state(operation_id,last_seq,blocked) VALUES(?,0,0)`, r.OperationID); err != nil {
			return Record{}, ErrStorage
		}
	}
	if err := tx.Commit(); err != nil {
		return Record{}, ErrStorage
	}
	return r, nil
}
