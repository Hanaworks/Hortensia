package operations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"docker-manager/internal/protocol"
	"modernc.org/sqlite"
)

//go:embed migrations/001_operations.sql
var schemaV1 string

// Store owns independent copies of its keys and must not be copied. Open on
// the same database changes the durable active boot and revokes older writers.
type Store struct {
	db        *sql.DB
	boot      string
	identity  Identity
	keys      Keys
	validator *protocol.Validator
	mu        sync.Mutex
	closing   bool
	closeDone chan struct{}
	closeErr  error
}

func init() {
	driver := &sqlite.Driver{}
	driver.RegisterConnectionHook(verifyConnection)
	sql.Register("manager-operations-sqlite", driver)
}

func verifyConnection(conn sqlite.ExecQuerierContext, _ string) error {
	for query, want := range map[string]string{"SELECT sqlite_version()": "3.54.1", "PRAGMA journal_mode": "wal", "PRAGMA synchronous": "2", "PRAGMA foreign_keys": "1", "PRAGMA busy_timeout": "5000"} {
		rows, err := conn.QueryContext(context.Background(), query, nil)
		if err != nil {
			return ErrStorage
		}
		values := make([]driver.Value, 1)
		nextErr, closeErr := rows.Next(values), rows.Close()
		if nextErr != nil || closeErr != nil || fmt.Sprint(values[0]) != want {
			return ErrStorage
		}
	}
	return nil
}

func localPath(path string) (string, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(strings.ToLower(path), "file:") || strings.ContainsAny(path, "?\x00") || strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
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

func openDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("manager-operations-sqlite", databaseURI(path))
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

func validKeyID(id string) bool { return id != "" && len(id) <= 128 && utf8.ValidString(id) }
func validateKeys(keys Keys) error {
	if !validKeyID(keys.DigestID) || !validKeyID(keys.EncryptionID) || keys.DigestID == keys.EncryptionID || len(keys.DigestKey) != 32 || len(keys.EncryptionKey) != 32 || hmac.Equal(keys.DigestKey, keys.EncryptionKey) {
		return ErrKey
	}
	return nil
}

func keyCheck(manager, domain, id string, key []byte) []byte {
	raw, _ := json.Marshal([]string{domain, manager, id})
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	return mac.Sum(nil)
}

func newStore(db *sql.DB, identity Identity, keys Keys) (*Store, error) {
	boot, err := protocol.NewID()
	if err != nil {
		return nil, ErrStorage
	}
	validator, err := protocol.NewValidator()
	if err != nil {
		return nil, ErrSchema
	}
	owned := Keys{DigestID: keys.DigestID, EncryptionID: keys.EncryptionID, DigestKey: append([]byte(nil), keys.DigestKey...), EncryptionKey: append([]byte(nil), keys.EncryptionKey...)}
	return &Store{db: db, boot: boot, identity: identity, keys: owned, validator: validator, closeDone: make(chan struct{})}, nil
}

func normalizeSchemaSQL(statement string) string {
	return strings.TrimSpace(strings.ReplaceAll(statement, "\r\n", "\n"))
}

// Exact app-owned SQL prevents a database with removed constraints from being
// mistaken for the supported schema. SQLite's own internal objects are ignored.
func validateSchema(ctx context.Context, db *sql.DB) error {
	expected := make(map[string]string)
	for _, statement := range strings.Split(schemaV1, ";") {
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

// Create exclusively creates a new controlled database. A failed creation is
// deliberately left in place; callers must not silently replace uncertain data.
func Create(ctx context.Context, path, managerID string, keys Keys) (*Store, error) {
	if !validID(managerID) {
		return nil, ErrIdentity
	}
	if err := validateKeys(keys); err != nil {
		return nil, err
	}
	full, err := localPath(path)
	if err != nil {
		return nil, err
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
	identity := Identity{ManagerID: managerID, DigestKeyID: keys.DigestID, EncryptionKeyID: keys.EncryptionID}
	s, err := newStore(db, identity, keys)
	if err != nil {
		db.Close()
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			s.Close()
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
		return nil, ErrStorage
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO manager_meta(singleton,schema_version,manager_id,active_boot,recovery_mode) VALUES(1,1,?,?,0)`, managerID, s.boot); err != nil {
		return nil, ErrStorage
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO keys(singleton,digest_key_id,encryption_key_id,digest_key_check,encryption_key_check,uses) VALUES(1,?,?,?,?,0)`, keys.DigestID, keys.EncryptionID, keyCheck(managerID, "dm-manager-digest-key-v1", keys.DigestID, keys.DigestKey), keyCheck(managerID, "dm-manager-encryption-key-v1", keys.EncryptionID, keys.EncryptionKey)); err != nil {
		return nil, ErrStorage
	}
	if err := tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	success = true
	return s, nil
}

// Open accepts only a matching complete database. It atomically enters recovery
// mode, freezes nodes, and prevents older Store boots from making more writes.
// This is ordinary restart handling, not protection against backup rollback.
func Open(ctx context.Context, path string, expected Identity, keys Keys) (*Store, error) {
	if !validID(expected.ManagerID) || !validKeyID(expected.DigestKeyID) || !validKeyID(expected.EncryptionKeyID) {
		return nil, ErrIdentity
	}
	if err := validateKeys(keys); err != nil {
		return nil, err
	}
	if keys.DigestID != expected.DigestKeyID || keys.EncryptionID != expected.EncryptionKeyID {
		return nil, ErrKey
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
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return nil, ErrStorage
	}
	if version != 1 {
		return nil, ErrSchema
	}
	if err := validateSchema(ctx, db); err != nil {
		return nil, err
	}
	var actual Identity
	var schemaVersion int
	if err := db.QueryRowContext(ctx, `SELECT manager_id,schema_version FROM manager_meta WHERE singleton=1`).Scan(&actual.ManagerID, &schemaVersion); err != nil {
		return nil, ErrSchema
	}
	if schemaVersion != 1 {
		return nil, ErrSchema
	}
	if actual.ManagerID != expected.ManagerID {
		return nil, ErrIdentity
	}
	var digestCheck, encryptionCheck []byte
	var uses int64
	if err := db.QueryRowContext(ctx, `SELECT digest_key_id,encryption_key_id,digest_key_check,encryption_key_check,uses FROM keys WHERE singleton=1`).Scan(&actual.DigestKeyID, &actual.EncryptionKeyID, &digestCheck, &encryptionCheck, &uses); err != nil {
		return nil, ErrKey
	}
	if actual.DigestKeyID != keys.DigestID || actual.EncryptionKeyID != keys.EncryptionID || !hmac.Equal(digestCheck, keyCheck(actual.ManagerID, "dm-manager-digest-key-v1", keys.DigestID, keys.DigestKey)) || !hmac.Equal(encryptionCheck, keyCheck(actual.ManagerID, "dm-manager-encryption-key-v1", keys.EncryptionID, keys.EncryptionKey)) {
		return nil, ErrKey
	}
	if uses < 0 || uses > maxKeyUses {
		return nil, ErrEvidence
	}
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return nil, ErrStorage
	}
	s, err := newStore(db, actual, keys)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			s.Close()
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrStorage
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE manager_meta SET active_boot=?,recovery_mode=1 WHERE singleton=1`, s.boot); err != nil {
		return nil, ErrStorage
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET frozen=1`); err != nil {
		return nil, ErrStorage
	}
	if err := tx.Commit(); err != nil {
		return nil, ErrStorage
	}
	success = true
	return s, nil
}

func (s *Store) Identity() Identity { return s.identity }

// requireActive belongs inside every writing transaction. Reading historical
// evidence through an older handle remains safe; issuing new facts does not.
func (s *Store) requireActive(ctx context.Context, q querier) error {
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return ErrStorage
	}
	var boot string
	if err := q.QueryRowContext(ctx, `SELECT active_boot FROM manager_meta WHERE singleton=1`).Scan(&boot); err != nil {
		return ErrStorage
	}
	if boot != s.boot {
		return ErrFrozen
	}
	return nil
}

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
	s.mu.Unlock()
	err := s.db.Close()
	if err != nil {
		err = ErrStorage
	}
	s.mu.Lock()
	clear(s.keys.DigestKey)
	clear(s.keys.EncryptionKey)
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}
