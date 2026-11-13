// Package snapshotcheck owns read-only snapshot resources, never business boot.
package snapshotcheck

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"docker-manager/internal/localstate"
	"modernc.org/sqlite"
)

var ErrInput = errors.New("snapshot inspection input invalid")
var ErrStorage = errors.New("snapshot inspection storage unavailable")
var ErrChanged = errors.New("snapshot inspection input changed")
var ErrLimit = errors.New("snapshot inspection budget exceeded")

type Reader struct {
	ctx              context.Context
	cancel           context.CancelFunc
	path             string
	info             os.FileInfo
	digest           [32]byte
	db               *sql.DB
	conn             *sql.Conn
	tx               *sql.Tx
	closed, finished bool
	closeErr, result error
}

func StorageError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return errors.Join(ErrStorage, ctx.Err())
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(ErrStorage, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrStorage, context.DeadlineExceeded)
	}
	for _, fixed := range []error{ErrInput, ErrChanged, ErrLimit, ErrStorage} {
		if errors.Is(err, fixed) {
			return fixed
		}
	}
	var code interface{ Code() int }
	if errors.As(err, &code) && code.Code()&255 == 18 {
		return ErrLimit
	}
	return ErrStorage
}

func localPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && !strings.HasPrefix(path, `\\`) && !strings.HasPrefix(path, "//") && !strings.ContainsAny(path, "?\x00#") && !strings.Contains(strings.TrimPrefix(path, filepath.VolumeName(path)), ":")
}
func checkFile(path string) (os.FileInfo, error) {
	if !localPath(path) || localstate.RejectLinks(path) != nil || localstate.CheckPrivateDir(filepath.Dir(path)) != nil {
		return nil, ErrInput
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !localstate.PrivateMode(info, false) || !singleLink(path, info) {
		return nil, ErrInput
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			return nil, ErrInput
		}
	}
	return info, nil
}
func fileHash(ctx context.Context, path string, expected os.FileInfo) (digest [32]byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return digest, ErrStorage
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = ErrStorage
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return digest, ErrStorage
	}
	if !os.SameFile(info, expected) || info.Size() != expected.Size() {
		return digest, ErrChanged
	}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return digest, err
		}
		var n int
		n, err = f.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return digest, ErrStorage
		}
	}
	if err = ctx.Err(); err != nil {
		return digest, err
	}
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

func Open(parent context.Context, path string) (result *Reader, err error) {
	if parent == nil {
		return nil, ErrInput
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	r := &Reader{ctx: ctx, cancel: cancel, path: path}
	defer func() {
		if err != nil {
			_ = r.Close()
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if r.info, err = checkFile(path); err != nil {
		return nil, err
	}
	if r.digest, err = fileHash(ctx, path, r.info); err != nil {
		return nil, err
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	u.RawQuery = url.Values{"mode": {"ro"}, "immutable": {"1"}, "_pragma": {"query_only(1)", "busy_timeout(0)", "foreign_keys(1)"}}.Encode()
	if r.db, err = sql.Open("sqlite", u.String()); err != nil {
		return nil, StorageError(ctx, err)
	}
	r.db.SetMaxOpenConns(1)
	r.db.SetMaxIdleConns(0)
	if r.conn, err = r.db.Conn(ctx); err != nil {
		return nil, StorageError(ctx, err)
	}
	err = r.conn.Raw(func(raw any) error {
		ro, ok := raw.(interface{ IsReadOnly(string) (bool, error) })
		if !ok {
			return ErrStorage
		}
		value, e := ro.IsReadOnly("main")
		if e != nil || !value {
			return ErrStorage
		}
		return nil
	})
	if err != nil {
		return nil, StorageError(ctx, err)
	}
	if _, err = sqlite.Limit(r.conn, 0, 32<<20); err != nil {
		return nil, StorageError(ctx, err)
	}
	if r.tx, err = r.conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err != nil {
		return nil, StorageError(ctx, err)
	}
	var queryOnly int
	if err = r.tx.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return nil, StorageError(ctx, err)
	}
	if queryOnly != 1 {
		return nil, ErrStorage
	}
	return r, nil
}
func (r *Reader) Context() context.Context { return r.ctx }
func (r *Reader) Tx() *sql.Tx              { return r.tx }
func (r *Reader) closeResources() error {
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	failed := false
	if r.tx != nil {
		err := r.tx.Rollback()
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			failed = true
		}
		r.tx = nil
	}
	if r.conn != nil {
		err := r.conn.Close()
		if err != nil && !errors.Is(err, sql.ErrConnDone) {
			failed = true
		}
		r.conn = nil
	}
	if r.db != nil {
		if r.db.Close() != nil {
			failed = true
		}
		r.db = nil
	}
	if failed {
		r.closeErr = ErrStorage
	}
	return r.closeErr
}
func (r *Reader) Finish() error {
	if r == nil {
		return ErrInput
	}
	if r.finished {
		return r.result
	}
	if r.closed {
		return ErrStorage
	}
	r.finished = true
	defer r.cancel()
	if err := r.closeResources(); err != nil {
		r.result = err
		return err
	}
	if err := r.ctx.Err(); err != nil {
		r.result = err
		return err
	}
	info, err := checkFile(r.path)
	if err != nil || !os.SameFile(r.info, info) || info.Size() != r.info.Size() {
		r.result = ErrChanged
		return r.result
	}
	digest, err := fileHash(r.ctx, r.path, info)
	if err != nil {
		r.result = StorageError(r.ctx, err)
		return r.result
	}
	if digest != r.digest {
		r.result = ErrChanged
		return r.result
	}
	return nil
}
func (r *Reader) Close() error {
	if r == nil {
		return nil
	}
	if r.cancel != nil {
		r.cancel()
	}
	return r.closeResources()
}
