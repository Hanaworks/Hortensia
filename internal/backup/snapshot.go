// Package backup provides the SQLite copy primitive; it does not publish backups.
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"docker-manager/internal/localstate"
	"modernc.org/sqlite"
)

var (
	ErrInput       = errors.New("snapshot input invalid")
	ErrDestination = errors.New("snapshot destination unavailable")
	ErrPartial     = errors.New("snapshot staging is partial")
)

const snapshotTimeout = 5 * time.Minute

// Snapshot copies a live SQLite database, including committed WAL content, to a
// new file in a new private staging directory. The directory containing
// targetFile must not exist; its parent must already be private. Any failure
// after staging creation retains the partial files and wraps ErrPartial.
// Success only produces a database copy, never a complete backup or READY mark.
func Snapshot(ctx context.Context, sourceFile, targetFile string) (result error) {
	if ctx == nil {
		return ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !localFile(sourceFile) || validateSource(sourceFile) != nil {
		return ErrInput
	}
	if !localFile(targetFile) || localstate.RejectLinks(targetFile) != nil {
		return ErrDestination
	}
	stage := filepath.Dir(targetFile)
	if localstate.CheckPrivateDir(filepath.Dir(stage)) != nil {
		return ErrDestination
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Mkdir and O_EXCL own these names. Never reuse a partial attempt or a prior
	// valid backup, even when an existing directory looks empty.
	if err := os.Mkdir(stage, 0700); err != nil {
		return ErrDestination
	}
	defer func() {
		if result != nil {
			result = errors.Join(ErrPartial, result)
		}
	}()
	file, err := os.OpenFile(targetFile, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrDestination
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("reserve target: %w", err)
	}
	if err := copyDatabase(ctx, sourceFile, targetFile); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err = os.OpenFile(targetFile, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open completed snapshot: %w", err)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("sync snapshot: %w", err)
	}
	if err := localstate.SyncDirectory(stage); err != nil {
		return fmt.Errorf("sync snapshot directory: %w", err)
	}
	if err := localstate.SyncDirectory(filepath.Dir(stage)); err != nil {
		return fmt.Errorf("sync staging parent: %w", err)
	}
	return ctx.Err()
}

func localFile(path string) bool {
	if path == "" || !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?\x00#") {
		return false
	}
	// Reject alternate streams while preserving a Windows drive letter.
	return !strings.Contains(strings.TrimPrefix(path, filepath.VolumeName(path)), ":")
}

func validateSource(path string) error {
	if localstate.RejectLinks(path) != nil {
		return ErrInput
	}
	if err := singleRegularFile(path); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		name := path + suffix
		if _, err := os.Lstat(name); os.IsNotExist(err) {
			continue
		} else if err != nil || localstate.RejectLinks(name) != nil || singleRegularFile(name) != nil {
			return ErrInput
		}
	}
	return nil
}

func databaseURI(path, mode string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(0)"}}
	if mode == "rw" {
		q.Add("_pragma", "synchronous(FULL)")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func copyDatabase(ctx context.Context, sourceFile, targetFile string) (result error) {
	// The generic driver has no business-store connection hook. mode=ro reads
	// the live WAL; immutable would silently omit newer committed content.
	db, err := sql.Open("sqlite", databaseURI(sourceFile, "ro"))
	if err != nil {
		return fmt.Errorf("open snapshot source: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer func() { result = errors.Join(result, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect snapshot source: %w", err)
	}
	defer func() { result = errors.Join(result, conn.Close()) }()
	return conn.Raw(func(raw any) error {
		creator, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("sqlite online backup unavailable")
		}
		var backup *sqlite.Backup
		delay := 50 * time.Millisecond
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			backup, err = creator.NewBackup(databaseURI(targetFile, "rw"))
			if err == nil {
				break
			}
			if !retryable(err) {
				return fmt.Errorf("initialize snapshot: %w", err)
			}
			if err := pause(ctx, delay); err != nil {
				return err
			}
			delay = nextDelay(delay)
		}
		return copyPages(ctx, backup)
	})
}

type pageBackup interface {
	Step(int32) (bool, error)
	Finish() error
}

// copyPages owns Finish on every path; no caller may reuse this handle.
func copyPages(ctx context.Context, backup pageBackup) (result error) {
	defer func() {
		if err := backup.Finish(); err != nil {
			result = errors.Join(result, fmt.Errorf("finish snapshot: %w", err))
		}
	}()
	delay := 50 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		more, err := backup.Step(128)
		if err != nil {
			if !retryable(err) {
				return fmt.Errorf("copy snapshot pages: %w", err)
			}
			if err := pause(ctx, delay); err != nil {
				return err
			}
			delay = nextDelay(delay)
			continue
		}
		if !more {
			return ctx.Err()
		}
		delay = 50 * time.Millisecond
	}
}

func retryable(err error) bool {
	var code interface{ Code() int }
	if !errors.As(err, &code) {
		return false
	}
	base := code.Code() & 0xff
	return base == 5 || base == 6 // SQLITE_BUSY or SQLITE_LOCKED, including extended codes.
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextDelay(delay time.Duration) time.Duration {
	return min(delay*2, 500*time.Millisecond)
}
