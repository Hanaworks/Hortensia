// Package ledger is the controlled, local Agent start ledger. It records no
// configuration bodies and does not expose a production deployment handler.
package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"docker-manager/internal/protocol"
	"docker-manager/internal/startgate"
	"modernc.org/sqlite"
)

var (
	ErrInput            = errors.New("ledger input invalid")
	ErrIdentity         = errors.New("ledger identity mismatch")
	ErrKey              = errors.New("ledger key invalid")
	ErrSchema           = errors.New("ledger schema unsupported")
	ErrStorage          = errors.New("ledger storage failure")
	ErrNotFound         = errors.New("ledger record not found")
	ErrConflict         = errors.New("ledger fixed input conflict")
	ErrClaimed          = errors.New("ledger resource claimed")
	ErrFrozen           = errors.New("ledger operation frozen")
	ErrGateExists       = errors.New("ledger gate already exists")
	ErrAlreadyCommitted = errors.New("ledger start already committed")
	ErrJournal          = errors.New("ledger journal unavailable")
	ErrEvidence         = errors.New("ledger evidence invalid")
	ErrKeyExhausted     = errors.New("ledger journal key exhausted")
)

const (
	PhaseReady          = "ready"
	PhaseStartCommitted = "START_COMMITTED"
)

//go:embed migrations/001_start_ledger.sql
var schemaV1 string

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func init() {
	driver := &sqlite.Driver{}
	driver.RegisterConnectionHook(verifyConnection)
	sql.Register("agent-ledger-sqlite", driver)
}

// verifyConnection reads back every required setting after each physical
// connection is configured, including replacements created by database/sql.
func verifyConnection(conn sqlite.ExecQuerierContext, _ string) error {
	for query, want := range map[string]string{"SELECT sqlite_version()": "3.54.1", "PRAGMA journal_mode": "wal", "PRAGMA synchronous": "2", "PRAGMA foreign_keys": "1", "PRAGMA busy_timeout": "5000"} {
		rows, err := conn.QueryContext(context.Background(), query, nil)
		if err != nil {
			return ErrStorage
		}
		values := make([]driver.Value, 1)
		nextErr := rows.Next(values)
		closeErr := rows.Close()
		if nextErr != nil || closeErr != nil || fmt.Sprint(values[0]) != want {
			return ErrStorage
		}
	}
	return nil
}

type Identity struct{ NodeID, LedgerID, KeyID string }

// Store must not be copied. Each instance owns a fresh boot and all gates it
// creates; opening another Store does not revoke an existing one.
type Store struct {
	db        *sql.DB
	boot      string
	identity  Identity
	key       []byte
	journal   *journalConfig
	validator *protocol.Validator
	mu        sync.Mutex
	gates     map[string]*startgate.Gate
	closing   bool
	closeDone chan struct{}
	closeErr  error
}

func validID(value string) bool { return idPattern.MatchString(value) }

func validDigest(value string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == value
}

func validateIdentity(identity Identity, requireLedger bool) error {
	if !validID(identity.NodeID) || (requireLedger && !validID(identity.LedgerID)) || identity.KeyID == "" || len(identity.KeyID) > 128 || !utf8.ValidString(identity.KeyID) {
		return ErrIdentity
	}
	return nil
}

func localPath(path string) (string, error) {
	// Accept filesystem paths only: never interpret caller-provided DSN options.
	if path == "" || path == ":memory:" || strings.HasPrefix(strings.ToLower(path), "file:") || strings.ContainsAny(path, "?\x00") || strings.HasPrefix(path, "\\\\") || strings.HasPrefix(path, "//") {
		return "", ErrInput
	}
	full, err := filepath.Abs(path)
	if err != nil {
		return "", ErrInput
	}
	return full, nil
}

func databaseURI(path string) string {
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	q := u.Query()
	q.Set("mode", "rw")
	q.Set("_txlock", "immediate")
	for _, setting := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"} {
		q.Add("_pragma", setting)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// openDB configures pragmas in the DSN so they apply to replacement connections
// too. A connection limit of one keeps each Store's transactional reads ordered.
func openDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("agent-ledger-sqlite", databaseURI(path))
	if err != nil {
		return nil, ErrStorage
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, ErrStorage
	}
	return db, nil
}

func keyCheck(identity Identity, key []byte) []byte {
	raw, _ := json.Marshal([]string{identity.NodeID, identity.LedgerID, identity.KeyID})
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("docker-manager/ledger-key/v1\x00"))
	mac.Write(raw)
	return mac.Sum(nil)
}

func newStore(db *sql.DB, identity Identity, key []byte) (*Store, error) {
	boot, err := protocol.NewID()
	if err != nil {
		return nil, ErrStorage
	}
	validator, err := protocol.NewValidator()
	if err != nil {
		return nil, ErrSchema
	}
	return &Store{db: db, boot: boot, identity: identity, key: append([]byte(nil), key...), validator: validator, gates: make(map[string]*startgate.Gate), closeDone: make(chan struct{})}, nil
}

// validateSchema accepts precisely the app-owned objects in this version's
// embedded migration. Column presence alone cannot establish PK/FK/CHECK/STRICT
// constraints. SQLite's own indexes and internal objects are implementation
// details and are excluded. This is a version check, not a general SQL parser.
func validateSchema(ctx context.Context, db *sql.DB, schema string) error {
	expected := make(map[string]string)
	for _, statement := range strings.Split(schema, ";") {
		statement = normalizeSchemaSQL(statement)
		fields := strings.Fields(statement)
		if len(fields) >= 3 && fields[0] == "CREATE" && (fields[1] == "TABLE" || fields[1] == "INDEX") {
			expected[fields[2]] = statement
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT name,sql FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'`)
	if err != nil {
		return ErrStorage
	}
	defer rows.Close()
	for rows.Next() {
		var name, statement string
		if err := rows.Scan(&name, &statement); err != nil {
			return ErrStorage
		}
		want, exists := expected[name]
		if !exists || normalizeSchemaSQL(statement) != want {
			return ErrSchema
		}
		delete(expected, name)
	}
	if err := rows.Err(); err != nil {
		return ErrStorage
	}
	if len(expected) != 0 {
		return ErrSchema
	}
	return nil
}

func normalizeSchemaSQL(statement string) string {
	// Git checkouts may use CRLF or LF. This does not change the schema;
	// all other statement content remains subject to exact comparison.
	return strings.TrimSpace(strings.ReplaceAll(statement, "\r\n", "\n"))
}

// Create exclusively creates a new file. Any failure leaves that file in place;
// a caller must never repair an uncertain ledger by creating a replacement.
func Create(ctx context.Context, path, nodeID, keyID string, key []byte) (*Store, error) {
	return createStore(ctx, path, nodeID, keyID, key, nil)
}

func createStore(ctx context.Context, path, nodeID, keyID string, key []byte, journalKey *JournalKey) (*Store, error) {
	identity := Identity{NodeID: nodeID, KeyID: keyID}
	if err := validateIdentity(identity, false); err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, ErrKey
	}
	version, schema := 1, schemaV1
	if journalKey != nil {
		if err := validateJournalKey(*journalKey, key); err != nil {
			return nil, err
		}
		version, schema = 2, schemaV2
	}
	full, err := localPath(path)
	if err != nil {
		return nil, err
	}
	identity.LedgerID, err = protocol.NewID()
	if err != nil {
		return nil, ErrStorage
	}
	file, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrStorage
	}
	if err := file.Close(); err != nil {
		return nil, ErrStorage
	}
	db, err := openDB(ctx, full)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			db.Close()
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return nil, ErrStorage
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_meta(id,node_id,ledger_id,schema_version,ledger_key_id,key_check) VALUES(1,?,?,?,?,?)`, identity.NodeID, identity.LedgerID, version, identity.KeyID, keyCheck(identity, key)); err != nil {
		return nil, ErrStorage
	}
	if journalKey != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_keys(id,key_id,key_check,uses) VALUES(1,?,?,0)`, journalKey.ID, journalKeyCheck(identity, *journalKey)); err != nil {
			return nil, ErrStorage
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	s, err := newStore(db, identity, key)
	if err != nil {
		return nil, err
	}
	if journalKey != nil {
		s.journal = &journalConfig{keyID: journalKey.ID, kek: append([]byte(nil), journalKey.Key...)}
	}
	success = true
	return s, nil
}

// Open refuses absent, unrecognized, or mismatched ledgers and never migrates.
func Open(ctx context.Context, path string, expected Identity, key []byte) (*Store, error) {
	return openStore(ctx, path, expected, key, nil)
}

func openStore(ctx context.Context, path string, expected Identity, key []byte, journalKey *JournalKey) (*Store, error) {
	if err := validateIdentity(expected, true); err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, ErrKey
	}
	wantVersion, schema := 1, schemaV1
	if journalKey != nil {
		if err := validateJournalKey(*journalKey, key); err != nil {
			return nil, err
		}
		wantVersion, schema = 2, schemaV2
	}
	full, err := localPath(path)
	if err != nil {
		return nil, err
	}
	db, err := openDB(ctx, full)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			db.Close()
		}
	}()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, ErrStorage
	}
	if version != wantVersion {
		return nil, ErrSchema
	}
	var actual Identity
	var check []byte
	var schemaVersion int
	if err := db.QueryRowContext(ctx, `SELECT node_id,ledger_id,ledger_key_id,schema_version,key_check FROM agent_meta WHERE id=1`).Scan(&actual.NodeID, &actual.LedgerID, &actual.KeyID, &schemaVersion, &check); err != nil {
		return nil, ErrSchema
	}
	if schemaVersion != wantVersion {
		return nil, ErrSchema
	}
	if actual != expected {
		return nil, ErrIdentity
	}
	if !hmac.Equal(check, keyCheck(actual, key)) {
		return nil, ErrKey
	}
	if err := validateSchema(ctx, db, schema); err != nil {
		return nil, err
	}
	if journalKey != nil {
		var actualKeyID string
		var journalCheck []byte
		if err := db.QueryRowContext(ctx, `SELECT key_id,key_check FROM event_keys WHERE id=1`).Scan(&actualKeyID, &journalCheck); err != nil {
			return nil, ErrKey
		}
		if actualKeyID != journalKey.ID || !hmac.Equal(journalCheck, journalKeyCheck(actual, *journalKey)) {
			return nil, ErrKey
		}
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return nil, ErrStorage
	}
	s, err := newStore(db, actual, key)
	if err != nil {
		return nil, err
	}
	if journalKey != nil {
		s.journal = &journalConfig{keyID: journalKey.ID, kek: append([]byte(nil), journalKey.Key...)}
	}
	success = true
	return s, nil
}

func (s *Store) Identity() Identity { return s.identity }

// Close prevents new registrations, invalidates gates without holding the
// registration lock, and waits for any short Start callback before closing.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closing {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closing = true
	gates := make([]*startgate.Gate, 0, len(s.gates))
	for _, gate := range s.gates {
		gates = append(gates, gate)
	}
	s.mu.Unlock()
	for _, gate := range gates {
		gate.Invalidate()
	}
	err := s.db.Close()
	if err != nil {
		err = ErrStorage
	}
	s.mu.Lock()
	clear(s.key)
	if s.journal != nil {
		clear(s.journal.kek)
	}
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}
