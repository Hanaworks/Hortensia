package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var symlinkCapability struct {
	once sync.Once
	err  error
}

func createTestSymlink(t *testing.T, source, link string) {
	t.Helper()
	symlinkCapability.once.Do(func() {
		probe := link + ".capability"
		symlinkCapability.err = os.Symlink(source, probe)
		if symlinkCapability.err == nil {
			symlinkCapability.err = os.Remove(probe)
		}
	})
	if symlinkCapability.err != nil {
		t.Skipf("symlink creation unavailable (one probe per package): %v", symlinkCapability.err)
	}
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
}

func testURI(path, mode string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(0)"}}
	u.RawQuery = q.Encode()
	return u.String()
}

func fixture(t *testing.T, wal bool) (*sql.DB, string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source.sqlite")
	db, err := sql.Open("sqlite", testURI(source, "rwc"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if wal {
		execSQL(t, db, "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0")
	}
	execSQL(t, db, `CREATE TABLE boot (active_boot TEXT, session TEXT);
		INSERT INTO boot VALUES ('same-boot', 'same-session');
		CREATE TABLE packages (id INTEGER PRIMARY KEY, version INTEGER NOT NULL);
		CREATE TABLE events (id INTEGER PRIMARY KEY REFERENCES packages(id), version INTEGER NOT NULL);
		CREATE TABLE claims (id INTEGER PRIMARY KEY REFERENCES packages(id), version INTEGER NOT NULL);
		INSERT INTO packages VALUES (1, 1); INSERT INTO events VALUES (1, 1); INSERT INTO claims VALUES (1, 1);`)
	return db, source, root
}

func execSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func readCopy(t *testing.T, target string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", testURI(target, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity = %q, %v", integrity, err)
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("snapshot has broken foreign keys", rows.Err())
	}
	return db
}

func assertBoot(t *testing.T, db *sql.DB) {
	t.Helper()
	var boot, session string
	if err := db.QueryRow("SELECT active_boot,session FROM boot").Scan(&boot, &session); err != nil || boot != "same-boot" || session != "same-session" {
		t.Fatalf("boot/session changed: %q/%q, %v", boot, session, err)
	}
}

// Copying the main file or opening a Store with startup writes breaks this test.
func TestSnapshotIncludesCommittedWALWithoutChangingSource(t *testing.T) {
	db, source, root := fixture(t, true)
	execSQL(t, db, "PRAGMA wal_checkpoint(TRUNCATE)")
	execSQL(t, db, "BEGIN; UPDATE packages SET version=9; UPDATE events SET version=9; UPDATE claims SET version=9; COMMIT")
	before := map[string][32]byte{}
	for _, name := range []string{source, source + "-wal"} {
		raw, err := os.ReadFile(name)
		if err != nil || len(raw) == 0 {
			t.Fatalf("expected active WAL fixture %s: %v", name, err)
		}
		before[name] = sha256.Sum256(raw)
	}
	target := filepath.Join(root, "new-snapshot", "copy.sqlite")
	if err := Snapshot(context.Background(), source, target); err != nil {
		t.Fatal(err)
	}
	// Check the primitive's output before a verifier opens the WAL-format
	// copy: SQLite readers may themselves create empty WAL/SHM sidecars.
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 || entries[0].Name() != "copy.sqlite" {
		t.Fatalf("unexpected published marker/sidecar: %v, %v", entries, err)
	}
	if err := os.Rename(target, target+".moved"); err != nil {
		t.Fatalf("snapshot retained a target handle: %v", err)
	}
	target += ".moved"
	copyDB := readCopy(t, target)
	var version int
	if err := copyDB.QueryRow("SELECT version FROM packages").Scan(&version); err != nil || version != 9 {
		t.Fatalf("committed WAL version = %d, %v", version, err)
	}
	assertBoot(t, db)
	assertBoot(t, copyDB)
	for name, want := range before {
		raw, err := os.ReadFile(name)
		if err != nil || sha256.Sum256(raw) != want {
			t.Fatalf("source persistent bytes changed: %s, %v", name, err)
		}
	}
	if err := copyDB.Close(); err != nil {
		t.Fatal(err)
	}
}

// Nontransactional copying can mix versions even if a tiny database opens.
func TestSnapshotConcurrentWriterKeepsTransactionalConsistency(t *testing.T) {
	db, source, root := fixture(t, true)
	execSQL(t, db, `CREATE TABLE padding (body BLOB); WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<8192) INSERT INTO padding SELECT zeroblob(4096) FROM n`)
	target := filepath.Join(root, "concurrent", "copy.sqlite")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var completed atomic.Bool
	var during atomic.Int64
	writer := make(chan error, 1)
	go func() {
		for {
			if _, err := os.Stat(target); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				writer <- ctx.Err()
				return
			case <-time.After(time.Millisecond):
			}
		}
		for n := 2; n <= 12; n++ {
			tx, err := db.BeginTx(ctx, nil)
			if err == nil {
				for _, table := range []string{"packages", "events", "claims"} {
					if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET version=?", n); err != nil {
						break
					}
				}
				if err == nil {
					err = tx.Commit()
				} else {
					tx.Rollback()
				}
			}
			if err != nil {
				writer <- err
				return
			}
			if !completed.Load() {
				during.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
		writer <- nil
	}()
	err := Snapshot(ctx, source, target)
	completed.Store(true)
	if writerErr := <-writer; writerErr != nil {
		t.Fatal(writerErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if during.Load() == 0 {
		t.Fatal("fixture did not commit any transaction while Snapshot ran")
	}
	copyDB := readCopy(t, target)
	var p, e, c, count int
	if err := copyDB.QueryRow("SELECT p.version,e.version,c.version FROM packages p JOIN events e USING(id) JOIN claims c USING(id)").Scan(&p, &e, &c); err != nil || p != e || e != c || p < 1 || p > 12 {
		t.Fatalf("inconsistent versions: %d/%d/%d, %v", p, e, c, err)
	}
	if err := copyDB.QueryRow("SELECT count(*) FROM padding").Scan(&count); err != nil || count != 8192 {
		t.Fatalf("padding rows %d, %v", count, err)
	}
	assertBoot(t, db)
}

func TestSnapshotRejectsExistingDestinationAndInvalidPaths(t *testing.T) {
	_, source, root := fixture(t, true)
	oldDir := filepath.Join(root, "previous")
	if err := os.Mkdir(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(oldDir, "copy.sqlite")
	old := []byte("previous backup stays unchanged")
	if err := os.WriteFile(oldFile, old, 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, source, target string }{
		{"previous-file", source, oldFile},
		{"existing-staging", source, filepath.Join(oldDir, "new.sqlite")},
		{"same-source", source, source},
		{"missing-source", filepath.Join(root, "missing"), filepath.Join(root, "no-source", "copy.sqlite")},
		{"directory-source", root, filepath.Join(root, "directory", "copy.sqlite")},
		{"relative-source", "relative.sqlite", filepath.Join(root, "relative", "copy.sqlite")},
		{"URI-source", testURI(source, "ro"), filepath.Join(root, "uri", "copy.sqlite")},
		{"URI-target", source, "file:copy.sqlite?mode=rwc"},
		{"query-target", source, filepath.Join(root, "query", "copy.sqlite?bad")},
		{"relative-target", source, "copy.sqlite"},
		{"shared-target", source, `\\server\share\stage\copy.sqlite`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Snapshot(context.Background(), tt.source, tt.target); !errors.Is(err, ErrInput) && !errors.Is(err, ErrDestination) {
				t.Fatalf("unsafe path was not explicitly rejected: %v", err)
			}
		})
	}
	raw, err := os.ReadFile(oldFile)
	if err != nil || !bytes.Equal(raw, old) {
		t.Fatal("previous backup overwritten", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) > 5 { // source, WAL, SHM, previous only
		t.Fatalf("invalid inputs created staging: %v, %v", entries, err)
	}
}

func TestSnapshotRejectsLinks(t *testing.T) {
	_, source, root := fixture(t, true)
	link := filepath.Join(root, "source-link.sqlite")
	if err := os.Link(source, link); err != nil {
		t.Fatal(err)
	}
	if err := Snapshot(context.Background(), link, filepath.Join(root, "hardlink", "copy.sqlite")); !errors.Is(err, ErrInput) {
		t.Fatalf("hardlinked source = %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	t.Run("WAL-hardlink", func(t *testing.T) {
		alias := filepath.Join(root, "wal-alias")
		if err := os.Link(source+"-wal", alias); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(alias)
		if err := Snapshot(context.Background(), source, filepath.Join(root, "linked-wal", "copy.sqlite")); !errors.Is(err, ErrInput) {
			t.Fatalf("hardlinked WAL = %v", err)
		}
	})
	// Native Windows often needs Developer Mode/admin for symlink creation;
	// the Windows junction test covers real reparse points without that grant.
	t.Run("source-symlink", func(t *testing.T) {
		createTestSymlink(t, source, link)
		defer os.Remove(link)
		if err := Snapshot(context.Background(), link, filepath.Join(root, "symlink", "copy.sqlite")); !errors.Is(err, ErrInput) {
			t.Fatalf("symlink source = %v", err)
		}
	})
	t.Run("directory-symlink", func(t *testing.T) {
		dirLink := filepath.Join(root, "dir-link")
		createTestSymlink(t, root, dirLink)
		defer os.Remove(dirLink)
		if err := Snapshot(context.Background(), source, filepath.Join(dirLink, "new", "copy.sqlite")); !errors.Is(err, ErrDestination) {
			t.Fatalf("linked parent = %v", err)
		}
	})
}

func TestSnapshotAlreadyCancelledCreatesNothing(t *testing.T) {
	db, source, root := fixture(t, true)
	target := filepath.Join(root, "cancelled", "copy.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Snapshot(ctx, source, target); !errors.Is(err, context.Canceled) || errors.Is(err, ErrPartial) {
		t.Fatalf("cancelled result = %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(target)); !os.IsNotExist(err) {
		t.Fatalf("pre-cancelled call created staging: %v", err)
	}
	assertBoot(t, db)
}

func TestSnapshotBusyDeadlinePreservesPartialAndAllowsNewAttempt(t *testing.T) {
	db, source, root := fixture(t, false)
	execSQL(t, db, "BEGIN EXCLUSIVE")
	target := filepath.Join(root, "partial", "copy.sqlite")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Snapshot(ctx, source, target)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrPartial) {
		t.Fatalf("busy timeout = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("backoff did not honor caller deadline")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("partial target not retained: %v", err)
	}
	execSQL(t, db, "ROLLBACK")
	if err := Snapshot(context.Background(), source, target); !errors.Is(err, ErrDestination) {
		t.Fatalf("partial attempt was reused: %v", err)
	}
	if err := Snapshot(context.Background(), source, filepath.Join(root, "retry", "copy.sqlite")); err != nil {
		t.Fatal(err)
	}
	assertBoot(t, db)
}

func TestSnapshotRetriesRealBusyUntilLockReleased(t *testing.T) {
	db, source, root := fixture(t, false)
	execSQL(t, db, "BEGIN EXCLUSIVE")
	target := filepath.Join(root, "busy-retry", "copy.sqlite")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Snapshot(ctx, source, target) }()
	select {
	case err := <-done:
		t.Fatalf("busy was not retried: %v", err)
	case <-time.After(140 * time.Millisecond):
	}
	execSQL(t, db, "ROLLBACK")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertBoot(t, readCopy(t, target))
}

func TestSnapshotCorruptSourceReturnsPartialWithoutPublication(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "corrupt.sqlite")
	raw := []byte("this is not a sqlite database")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "broken-copy", "copy.sqlite")
	start := time.Now()
	if err := Snapshot(context.Background(), source, target); err == nil || !errors.Is(err, ErrPartial) {
		t.Fatalf("corrupt copy = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("non-lock error was retried")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("corrupt source changed", err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 || entries[0].Name() != "copy.sqlite" {
		t.Fatalf("partial emitted publication metadata: %v, %v", entries, err)
	}
}

// This fault adapter only replaces the raw driver boundary where real disk
// exhaustion/Finish failure cannot be safely induced on this workstation.
type backupFault struct {
	steps     []error
	finishErr error
	cancel    context.CancelFunc
	calls     int
	finishes  int
}

func (b *backupFault) Step(pages int32) (bool, error) {
	b.calls++
	if pages != 128 {
		return false, errors.New("unbounded backup step")
	}
	if b.cancel != nil {
		b.cancel()
		return true, nil
	}
	if b.calls <= len(b.steps) {
		return false, b.steps[b.calls-1]
	}
	return false, nil
}

func (b *backupFault) Finish() error {
	b.finishes++
	return b.finishErr
}

type sqliteFault int

func (e sqliteFault) Error() string { return "controlled SQLite fault" }
func (e sqliteFault) Code() int     { return int(e) }

func TestCopyPagesPropagatesFinishFailure(t *testing.T) {
	want := errors.New("controlled finish failure")
	fault := &backupFault{finishErr: want}
	if err := copyPages(context.Background(), fault); !errors.Is(err, want) {
		t.Fatalf("finish failure lost: %v", err)
	}
	if fault.calls != 1 || fault.finishes != 1 {
		t.Fatalf("step/finish counts = %d/%d", fault.calls, fault.finishes)
	}
}

func TestCopyPagesStopsBetweenBoundedStepsAndFinishes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fault := &backupFault{cancel: cancel}
	if err := copyPages(ctx, fault); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if fault.calls != 1 || fault.finishes != 1 {
		t.Fatalf("step/finish counts = %d/%d", fault.calls, fault.finishes)
	}
}

func TestCopyPagesRetriesExtendedLockedButNotIOErrors(t *testing.T) {
	t.Run("extended-locked", func(t *testing.T) {
		fault := &backupFault{steps: []error{sqliteFault(6 | 1<<8)}}
		if err := copyPages(context.Background(), fault); err != nil {
			t.Fatal(err)
		}
		if fault.calls != 2 || fault.finishes != 1 {
			t.Fatalf("step/finish counts = %d/%d", fault.calls, fault.finishes)
		}
	})
	t.Run("io-error", func(t *testing.T) {
		want := sqliteFault(10) // SQLITE_IOERR, never retry as if it were BUSY.
		fault := &backupFault{steps: []error{want}}
		if err := copyPages(context.Background(), fault); !errors.Is(err, want) {
			t.Fatalf("IO error lost: %v", err)
		}
		if fault.calls != 1 || fault.finishes != 1 {
			t.Fatalf("step/finish counts = %d/%d", fault.calls, fault.finishes)
		}
	})
}
