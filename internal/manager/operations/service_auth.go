package operations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"docker-manager/internal/protocol"
	"golang.org/x/crypto/argon2"
	"modernc.org/sqlite"
)

func serviceStorageError(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 255 {
		case 5, 6:
			return serviceError(503, "storage_busy")
		case 13:
			return serviceError(503, "storage_full")
		}
	}
	return ErrStorage
}

func (s *Store) storageFailure(err error) error {
	if s.version == 4 || s.version == 5 {
		return serviceStorageError(err)
	}
	return ErrStorage
}

var passwordSlots = make(chan struct{}, 2)
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

type serviceBootstrap struct{ username, hash string }

func validPassword(p string) bool {
	return utf8.ValidString(p) && utf8.RuneCountInString(p) >= 12 && utf8.RuneCountInString(p) <= 128
}
func passwordKey(ctx context.Context, p string, salt []byte) ([]byte, error) {
	select {
	case passwordSlots <- struct{}{}:
		defer func() { <-passwordSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, serviceError(429, "rate_limited")
	}
	return argon2.IDKey([]byte(p), salt, 3, 64*1024, 1, 32), nil
}
func hashPassword(ctx context.Context, p string) (string, error) {
	if !validPassword(p) {
		return "", serviceError(400, "invalid_field")
	}
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return "", serviceStorageError(e)
	}
	key, e := passwordKey(ctx, p, salt)
	if e != nil {
		return "", e
	}
	defer clear(key)
	return "$argon2id$v=19$m=65536,t=3,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func checkPassword(ctx context.Context, p, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	salt := make([]byte, 16)
	want := make([]byte, 32)
	valid := len(parts) == 6 && len(encoded) == 97
	if valid {
		var e error
		salt, e = base64.RawStdEncoding.Strict().DecodeString(parts[4])
		valid = e == nil && len(salt) == 16
		want, e = base64.RawStdEncoding.Strict().DecodeString(parts[5])
		valid = valid && e == nil && len(want) == 32 && parts[1] == "argon2id" && parts[2] == "v=19" && parts[3] == "m=65536,t=3,p=1"
	}
	if !valid {
		salt = make([]byte, 16)
		want = make([]byte, 32)
	}
	actual, e := passwordKey(ctx, p, salt)
	if e != nil {
		return false, e
	}
	defer clear(actual)
	return subtle.ConstantTimeCompare(actual, want) == 1 && valid, nil
}
func CreateService(ctx context.Context, path, managerID string, keys Keys, b Bootstrap) (*Store, error) {
	if !usernamePattern.MatchString(b.Username) {
		return nil, serviceError(400, "invalid_field")
	}
	hash, e := hashPassword(ctx, b.Password)
	if e != nil {
		return nil, e
	}
	return createVersion(ctx, path, managerID, keys, 4, serviceBootstrap{b.Username, hash})
}
func OpenService(ctx context.Context, path string, expected Identity, keys Keys) (*Store, error) {
	return openVersion(ctx, path, expected, keys, 4)
}
func insertBootstrap(ctx context.Context, tx *sql.Tx, b serviceBootstrap) error {
	id, e := protocol.NewID()
	if e != nil {
		return serviceStorageError(e)
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO users(id,enabled,auth_rev,login_name,password_hash,system_admin,created_at) VALUES(?,1,0,?,?,1,?)`, id, b.username, b.hash, time.Now().UnixMilli())
	return e
}
func serviceError(status int, code string) *ServiceError {
	category, retry := "invalid_input", "none"
	switch status {
	case 401, 403:
		category = "permission_denied"
	case 404:
		category = "not_found"
	case 409, 412:
		category = "conflict"
		retry = "refresh_and_review"
	case 410, 428:
		retry = "refresh_and_review"
	case 422:
		category = "unsupported"
	case 429, 500, 503:
		category = "unavailable"
		retry = "same_request"
	}
	e := &ServiceError{Status: status, Code: code, Category: category, Message: code, RetryAction: retry, FieldErrors: []ServiceFieldError{}}
	if status == 429 {
		e.RetryAfter = 1
	}
	return e
}
func serviceReply(status int, body any) ServiceReply {
	raw, _ := json.Marshal(body)
	return ServiceReply{Status: status, Body: raw}
}
func randomSecret() (string, error) {
	raw := make([]byte, 32)
	if _, e := rand.Read(raw); e != nil {
		return "", serviceStorageError(e)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func secretHash(v string) []byte { sum := sha256.Sum256([]byte(v)); return sum[:] }

type serviceUser struct {
	id, name, hash string
	enabled, admin bool
	rev            uint64
	created        int64
}

func readServiceUser(ctx context.Context, q querier, id string) (serviceUser, error) {
	var u serviceUser
	e := q.QueryRowContext(ctx, `SELECT id,login_name,password_hash,enabled,system_admin,auth_rev,created_at FROM users WHERE id=?`, id).Scan(&u.id, &u.name, &u.hash, &u.enabled, &u.admin, &u.rev, &u.created)
	if errors.Is(e, sql.ErrNoRows) {
		return u, serviceError(404, "not_found")
	}
	if e != nil {
		return u, serviceStorageError(e)
	}
	return u, nil
}
func (s *Store) requireBrowserTx(ctx context.Context, tx *sql.Tx, b *BrowserSession) (serviceUser, error) {
	if s.version != 4 && s.version != 5 || b == nil || b.store != s {
		return serviceUser{}, serviceError(401, "unauthenticated")
	}
	if e := s.requireActive(ctx, tx); e != nil {
		return serviceUser{}, e
	}
	var user string
	var last, expires int64
	var revoked sql.NullInt64
	e := tx.QueryRowContext(ctx, `SELECT user_id,last_seen_at,expires_at,revoked_at FROM user_sessions WHERE token_hash=? AND id=?`, b.tokenHash, b.id).Scan(&user, &last, &expires, &revoked)
	now := time.Now().UnixMilli()
	if errors.Is(e, sql.ErrNoRows) {
		return serviceUser{}, serviceError(401, "unauthenticated")
	}
	if e != nil {
		return serviceUser{}, serviceStorageError(e)
	}
	if revoked.Valid || now >= expires || now >= last+30*60*1000 || user != b.user {
		return serviceUser{}, serviceError(401, "unauthenticated")
	}
	u, e := readServiceUser(ctx, tx, user)
	if e != nil {
		return u, e
	}
	if !u.enabled {
		return u, serviceError(401, "unauthenticated")
	}
	return u, nil
}
func userBody(u serviceUser) map[string]any {
	return map[string]any{"user_id": u.id, "username": u.name, "enabled": u.enabled, "auth_rev": u.rev}
}
func (s *Store) meTx(ctx context.Context, tx *sql.Tx, b *BrowserSession, u serviceUser) (ServiceReply, error) {
	var expires int64
	if e := tx.QueryRowContext(ctx, `SELECT expires_at FROM user_sessions WHERE id=?`, b.id).Scan(&expires); e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	caps := []string{}
	if u.admin {
		caps = append(caps, "system.admin")
	}
	r := serviceReply(200, map[string]any{"user_id": u.id, "username": u.name, "auth_rev": u.rev, "system_capabilities": caps, "csrf_token": b.csrf, "expires_at": timestamp(expires)})
	r.ETag = `"` + strconv.FormatUint(u.rev, 10) + `"`
	return r, nil
}
func (s *Store) Login(ctx context.Context, username, password string) (BrowserLogin, error) {
	if s.version != 4 && s.version != 5 {
		return BrowserLogin{}, ErrSchema
	}
	if !usernamePattern.MatchString(username) || !validPassword(password) {
		return BrowserLogin{}, serviceError(401, "invalid_credentials")
	}
	var id, hash string
	var rev uint64
	var enabled bool
	e := s.db.QueryRowContext(ctx, `SELECT id,password_hash,auth_rev,enabled FROM users WHERE login_name=?`, username).Scan(&id, &hash, &rev, &enabled)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return BrowserLogin{}, serviceStorageError(e)
	}
	good, e := checkPassword(ctx, password, hash)
	if e != nil {
		return BrowserLogin{}, e
	}
	if !good || !enabled {
		return BrowserLogin{}, serviceError(401, "invalid_credentials")
	}
	token, e := randomSecret()
	if e != nil {
		return BrowserLogin{}, e
	}
	csrf, e := randomSecret()
	if e != nil {
		return BrowserLogin{}, e
	}
	sid, e := protocol.NewID()
	if e != nil {
		return BrowserLogin{}, serviceStorageError(e)
	}
	b := &BrowserSession{store: s, id: sid, user: id, csrf: csrf, tokenHash: secretHash(token)}
	sealed, e := s.seal(ctx, blobBinding{OperationID: sid, NodeID: id, Digest: base64.RawURLEncoding.EncodeToString(b.tokenHash), Kind: "session-csrf"}, []byte(csrf))
	if e != nil {
		return BrowserLogin{}, e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return BrowserLogin{}, serviceStorageError(e)
	}
	defer tx.Rollback()
	if e = s.requireActive(ctx, tx); e != nil {
		return BrowserLogin{}, e
	}
	u, e := readServiceUser(ctx, tx, id)
	if e != nil {
		return BrowserLogin{}, e
	}
	if !u.enabled || u.hash != hash || u.rev != rev {
		return BrowserLogin{}, serviceError(401, "invalid_credentials")
	}
	now := time.Now().UnixMilli()
	_, e = tx.ExecContext(ctx, `INSERT INTO user_sessions(id,token_hash,user_id,auth_rev,created_at,last_seen_at,expires_at,csrf_cipher,wrapped_dek,key_id,fingerprint) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, sid, b.tokenHash, id, rev, now, now, now+12*60*60*1000, sealed.Cipher, sealed.WrappedDEK, sealed.KeyID, sealed.Fingerprint)
	if e != nil {
		return BrowserLogin{}, serviceStorageError(e)
	}
	r, e := s.meTx(ctx, tx, b, u)
	if e != nil {
		return BrowserLogin{}, e
	}
	if e = tx.Commit(); e != nil {
		return BrowserLogin{}, serviceStorageError(e)
	}
	return BrowserLogin{b, token, r}, nil
}
func (s *Store) AuthenticateBrowser(ctx context.Context, token string) (*BrowserSession, error) {
	if s.version != 4 && s.version != 5 || !validDigest(token) {
		return nil, serviceError(401, "unauthenticated")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, serviceStorageError(e)
	}
	defer tx.Rollback()
	b := &BrowserSession{store: s, tokenHash: secretHash(token)}
	var sealed sealedBlob
	e = tx.QueryRowContext(ctx, `SELECT id,user_id,csrf_cipher,wrapped_dek,key_id,fingerprint FROM user_sessions WHERE token_hash=?`, b.tokenHash).Scan(&b.id, &b.user, &sealed.Cipher, &sealed.WrappedDEK, &sealed.KeyID, &sealed.Fingerprint)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, serviceError(401, "unauthenticated")
	}
	if e != nil {
		return nil, serviceStorageError(e)
	}
	if _, e = s.requireBrowserTx(ctx, tx, b); e != nil {
		return nil, e
	}
	raw, e := s.unseal(blobBinding{OperationID: b.id, NodeID: b.user, Digest: base64.RawURLEncoding.EncodeToString(b.tokenHash), Kind: "session-csrf"}, sealed)
	if e != nil {
		return nil, e
	}
	b.csrf = string(raw)
	clear(raw)
	if !validDigest(b.csrf) {
		return nil, ErrEvidence
	}
	if _, e = tx.ExecContext(ctx, `UPDATE user_sessions SET last_seen_at=? WHERE id=?`, time.Now().UnixMilli(), b.id); e != nil {
		return nil, serviceStorageError(e)
	}
	if e = tx.Commit(); e != nil {
		return nil, serviceStorageError(e)
	}
	return b, nil
}
func (s *Store) BrowserMe(ctx context.Context, b *BrowserSession) (ServiceReply, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ServiceReply{}, serviceStorageError(e)
	}
	defer tx.Rollback()
	u, e := s.requireBrowserTx(ctx, tx, b)
	if e != nil {
		return ServiceReply{}, e
	}
	return s.meTx(ctx, tx, b, u)
}
func (s *Store) Logout(ctx context.Context, b *BrowserSession) error {
	if b == nil {
		return nil
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return serviceStorageError(e)
	}
	defer tx.Rollback()
	if _, e = s.requireBrowserTx(ctx, tx, b); e != nil {
		var se *ServiceError
		if errors.As(e, &se) && se.Status == 401 {
			return nil
		}
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at=? WHERE id=?`, time.Now().UnixMilli(), b.id); e != nil {
		return serviceStorageError(e)
	}
	if e = tx.Commit(); e != nil {
		return serviceStorageError(e)
	}
	return nil
}
