package ledger

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"docker-manager/internal/protocol"
)

//go:embed migrations/005_runtime.sql
var schemaV5 string

type runtimeConfig struct {
	now          func() time.Time
	mu           sync.Mutex
	entered      map[string]bool
	committed    map[string]bool
	commitFailed map[string]bool
}

func runtimeConfiguration(o RuntimeOptions) *runtimeConfig {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &runtimeConfig{now: now, entered: map[string]bool{}, committed: map[string]bool{}, commitFailed: map[string]bool{}}
}
func CreateRuntime(ctx context.Context, path, nodeID, keyID string, key []byte, journal JournalKey, options RuntimeOptions) (*Store, error) {
	recovery := RecoveryOptions{Now: options.Now}
	s, err := createStore(ctx, path, nodeID, keyID, key, &journal, storeFormat{version: 5, schema: schemaV5, recovery: &recovery, release: true})
	if err != nil {
		return nil, err
	}
	s.runtime = runtimeConfiguration(options)
	p := options.Profile
	if p.EndpointID == "" {
		p.EndpointID, err = protocol.NewID()
		if err != nil {
			s.Close()
			return nil, ErrStorage
		}
	}
	if !validID(p.EndpointID) || p.PolicyRevision > 9007199254740991 {
		s.Close()
		return nil, ErrInput
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO runtime_profile(id,endpoint_id,policy_revision,fingerprint,engine_id,docker_version,compose_version) VALUES(1,?,?,?,?,?,?)`, p.EndpointID, p.PolicyRevision, p.Fingerprint, p.EngineID, p.DockerVersion, p.ComposeVersion)
	if err != nil {
		s.Close()
		return nil, ErrStorage
	}
	return s, nil
}
func OpenRuntime(ctx context.Context, path string, identity Identity, key []byte, journal JournalKey, options RuntimeOptions) (*Store, error) {
	recovery := RecoveryOptions{Now: options.Now}
	s, err := openStore(ctx, path, identity, key, &journal, storeFormat{version: 5, schema: schemaV5, recovery: &recovery, release: true})
	if err != nil {
		return nil, err
	}
	s.runtime = runtimeConfiguration(options)
	if _, err = s.RuntimeProfile(ctx); err != nil {
		s.Close()
		return nil, err
	}
	if options.Profile.EngineID != "" {
		if err = s.BindRuntimeProfile(ctx, options.Profile); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) BootID() string { return s.boot }
func (s *Store) requireRuntime() error {
	if s.runtime == nil {
		return ErrSchema
	}
	return s.requireJournal()
}
func readRuntimeProfile(ctx context.Context, q querier) (RuntimeProfile, error) {
	var p RuntimeProfile
	if err := q.QueryRowContext(ctx, `SELECT endpoint_id,policy_revision,fingerprint,engine_id,docker_version,compose_version FROM runtime_profile WHERE id=1`).Scan(&p.EndpointID, &p.PolicyRevision, &p.Fingerprint, &p.EngineID, &p.DockerVersion, &p.ComposeVersion); err != nil {
		return p, ErrEvidence
	}
	return p, nil
}
func (s *Store) RuntimeProfile(ctx context.Context) (RuntimeProfile, error) {
	if err := s.requireRuntime(); err != nil {
		return RuntimeProfile{}, err
	}
	return readRuntimeProfile(ctx, s.db)
}
func (s *Store) BindRuntimeProfile(ctx context.Context, p RuntimeProfile) error {
	if err := s.requireRuntime(); err != nil {
		return err
	}
	if !validID(p.EndpointID) || !validDigest(p.Fingerprint) || p.EngineID == "" || p.DockerVersion == "" || p.ComposeVersion == "" || p.PolicyRevision > 9007199254740991 {
		return ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	old, err := readRuntimeProfile(ctx, tx)
	if err != nil {
		return err
	}
	if old.EndpointID != p.EndpointID || (old.EngineID != "" && old.EngineID != p.EngineID) {
		return ErrIdentity
	}
	if old == p {
		return nil
	}
	if old.EngineID != "" {
		var claims int
		if tx.QueryRowContext(ctx, `SELECT count(*) FROM local_claims`).Scan(&claims) != nil {
			return ErrStorage
		}
		if claims != 0 || p.PolicyRevision != old.PolicyRevision+1 {
			return ErrConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_profile SET policy_revision=?,fingerprint=?,engine_id=?,docker_version=?,compose_version=? WHERE id=1`, p.PolicyRevision, p.Fingerprint, p.EngineID, p.DockerVersion, p.ComposeVersion); err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) BindManagerSession(ctx context.Context, raw []byte, helloMessageID string) error {
	if err := s.requireRuntime(); err != nil {
		return err
	}
	env, err := s.validator.Validate(raw)
	if err != nil || env.Type != "welcome" || env.SessionID == nil || env.ReplyTo == nil || *env.ReplyTo != helloMessageID || !validID(helloMessageID) {
		return ErrInput
	}
	var body struct {
		Node, Session string
		Reconcile     bool
	}
	var p struct {
		Node      string `json:"node_id"`
		Session   string `json:"session_id"`
		Reconcile bool   `json:"reconcile_required"`
	}
	if json.Unmarshal(env.Body, &p) != nil {
		return ErrInput
	}
	body.Node, body.Session, body.Reconcile = p.Node, p.Session, p.Reconcile
	if body.Node != s.identity.NodeID || body.Session != *env.SessionID || !body.Reconcile {
		return ErrIdentity
	}
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	s.freezeRecoveryGates()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_control SET current_session=?,frozen=1 WHERE id=1`, body.Session); err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) DisconnectManagerSession(ctx context.Context, sessionID string) error {
	if err := s.requireRuntime(); err != nil {
		return err
	}
	s.recovery.mu.Lock()
	defer s.recovery.mu.Unlock()
	// A stale connection must never invalidate the replacement connection.
	if err := s.requireRecoverySession(ctx, s.db, sessionID); err != nil {
		return err
	}
	s.freezeRecoveryGates()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoverySession(ctx, tx, sessionID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE recovery_control SET current_session='',frozen=1 WHERE id=1`); err != nil {
		return ErrStorage
	}
	if err = tx.Commit(); err != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) sealDocument(ctx context.Context, r Record, kind string, seq uint64, value any) (sealedEvent, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return sealedEvent{}, ErrInput
	}
	return s.sealEvent(ctx, eventBinding{r.OperationID, r.Digest, "runtime-" + kind, seq}, raw)
}
func (s *Store) insertDocument(ctx context.Context, tx *sql.Tx, r Record, kind string, seq uint64, e sealedEvent) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_documents(operation_id,kind,seq,body_cipher,wrapped_dek,key_id,body_fingerprint) VALUES(?,?,?,?,?,?,?)`, r.OperationID, kind, seq, e.Cipher, e.WrappedDEK, e.KeyID, e.Fingerprint); err != nil {
		return ErrStorage
	}
	return nil
}
func (s *Store) readDocument(ctx context.Context, q querier, r Record, kind string, seq uint64, value any) error {
	var e sealedEvent
	err := q.QueryRowContext(ctx, `SELECT body_cipher,wrapped_dek,key_id,body_fingerprint FROM runtime_documents WHERE operation_id=? AND kind=? AND seq=?`, r.OperationID, kind, seq).Scan(&e.Cipher, &e.WrappedDEK, &e.KeyID, &e.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return ErrStorage
	}
	raw, err := s.openEvent(eventBinding{r.OperationID, r.Digest, "runtime-" + kind, seq}, e)
	if err != nil {
		return err
	}
	defer clear(raw)
	if json.Unmarshal(raw, value) != nil {
		return ErrEvidence
	}
	return nil
}
func (s *Store) PendingEventRefs(ctx context.Context, session string, limit int) ([]EventRef, error) {
	if err := s.requireRuntime(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 32 {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoverySession(ctx, tx, session); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT operation_id,kind,event_seq FROM execution_events WHERE ack_session IS NULL OR ack_session<>? ORDER BY operation_id,event_seq LIMIT ?`, session, limit)
	if err != nil {
		return nil, ErrStorage
	}
	defer rows.Close()
	refs := []EventRef{}
	for rows.Next() {
		var r EventRef
		if rows.Scan(&r.OperationID, &r.Kind, &r.Seq) != nil {
			return nil, ErrStorage
		}
		refs = append(refs, r)
	}
	if rows.Err() != nil {
		return nil, ErrStorage
	}
	return refs, nil
}

func (s *Store) quarantineRuntimeConflict(ctx context.Context, operation string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	if err = s.requireRecoveryWriter(ctx, tx); err != nil {
		return err
	}
	if err = s.quarantineRelease(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE event_state SET blocked=1 WHERE operation_id=?`, operation); err != nil {
		return ErrStorage
	}
	if err = s.noteRecoveryConflict(ctx, tx); err != nil {
		return err
	}
	if tx.Commit() != nil {
		return ErrStorage
	}
	return ErrConflict
}

// ReadProcessOutcome exposes only an independently durable supervisor fact.
// ErrNotFound means no known exit; it never permits attaching a historical PID.
func (s *Store) ReadProcessOutcome(ctx context.Context, operationID string) (ProcessOutcome, error) {
	if err := s.requireRuntime(); err != nil {
		return ProcessOutcome{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProcessOutcome{}, ErrStorage
	}
	defer tx.Rollback()
	r, err := s.readRecord(ctx, tx, operationID)
	if err != nil {
		return ProcessOutcome{}, err
	}
	return s.readProcessOutcome(ctx, tx, r)
}
func (s *Store) ReadPreparationEvidence(ctx context.Context, operationID string) (PreparationEvidence, error) {
	if err := s.requireRuntime(); err != nil {
		return PreparationEvidence{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PreparationEvidence{}, ErrStorage
	}
	defer tx.Rollback()
	r, err := s.readRecord(ctx, tx, operationID)
	if err != nil {
		return PreparationEvidence{}, err
	}
	var e PreparationEvidence
	if err = s.readDocument(ctx, tx, r, "preparation", 0, &e); err != nil {
		return e, err
	}
	if e.OperationID != r.OperationID || e.InputFingerprint != r.LocalFingerprint {
		return PreparationEvidence{}, ErrEvidence
	}
	return e, nil
}
