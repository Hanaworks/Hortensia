package snapshotcheck

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"docker-manager/internal/backup"
	_ "modernc.org/sqlite"
)

func testDB(t *testing.T, wal bool) (string, *sql.DB) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mode := "DELETE"
	if wal {
		mode = "WAL"
	}
	if _, err = db.Exec("PRAGMA journal_mode=" + mode + "; PRAGMA user_version=5; CREATE TABLE control(boot TEXT); INSERT INTO control VALUES('original-boot')"); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	return p, db
}

func TestReaderIsActuallyReadOnlyAndPreservesFiles(t *testing.T) {
	p, db := testDB(t, false)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var boot string
	if err = r.Tx().QueryRowContext(r.Context(), "SELECT boot FROM control").Scan(&boot); err != nil || boot != "original-boot" {
		t.Fatal(boot, err)
	}
	if _, err = r.Tx().ExecContext(r.Context(), "UPDATE control SET boot='changed'"); err == nil {
		t.Fatal("snapshot writable")
	}
	if err = r.Finish(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(p)
	if err != nil || string(before) != string(after) {
		t.Fatal("source changed", err)
	}
	files, err := os.ReadDir(filepath.Dir(p))
	if err != nil || len(files) != 1 {
		t.Fatal("sidecars created", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReaderRejectsUnsealedAndLinkedInput(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			p, db := testDB(t, false)
			db.Close()
			if err := os.WriteFile(p+suffix, []byte("evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(context.Background(), p); !errors.Is(err, ErrInput) {
				t.Fatal("unsealed source not rejected", err)
			}
			raw, err := os.ReadFile(p + suffix)
			if err != nil || string(raw) != "evidence" {
				t.Fatal("material changed", err)
			}
		})
	}
	p, db := testDB(t, false)
	db.Close()
	alias := p + "-alias"
	if err := os.Link(p, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), p); !errors.Is(err, ErrInput) {
		t.Fatal("hard link accepted", err)
	}
}

func TestReaderAcceptsCompleteCopyOfLiveWAL(t *testing.T) {
	p, db := testDB(t, true)
	if _, err := db.Exec("UPDATE control SET boot='committed-in-wal'"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "copy", "snapshot.db")
	if err := backup.Snapshot(context.Background(), p, target); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var boot string
	if err = r.Tx().QueryRowContext(r.Context(), "SELECT boot FROM control").Scan(&boot); err != nil || boot != "committed-in-wal" {
		t.Fatal(boot, err)
	}
	if err = r.Finish(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(files) != 1 {
		t.Fatal("copy file set changed", err)
	}
}

func TestReaderFinishDetectsChangedBytesAndSidecars(t *testing.T) {
	for _, sidecar := range []bool{false, true} {
		t.Run(map[bool]string{false: "bytes", true: "sidecar"}[sidecar], func(t *testing.T) {
			p, db := testDB(t, false)
			db.Close()
			r, err := Open(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			var version int
			if err = r.Tx().QueryRowContext(r.Context(), "PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if sidecar {
				err = os.WriteFile(p+"-wal", []byte("new writer"), 0600)
			} else {
				var f *os.File
				f, err = os.OpenFile(p, os.O_RDWR, 0)
				if err == nil {
					_, err = f.WriteAt([]byte{6}, 63)
					f.Close()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Finish(); !errors.Is(err, ErrChanged) {
				t.Fatal("changed source accepted", err)
			}
		})
	}
}

func TestReaderCancellationAndInvalidInput(t *testing.T) {
	if _, err := Open(nil, "x"); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	p, db := testDB(t, false)
	db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", p + "?query", p + "#fragment", "file:" + p} {
		if _, err := Open(context.Background(), path); !errors.Is(err, ErrInput) {
			t.Fatal(path, err)
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	r, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cancel()
	if err = r.Finish(); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel lost", err)
	}
}

func TestReaderBoundsSQLiteValuesAndKeepsErrorsPrivate(t *testing.T) {
	p, db := testDB(t, false)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var large []byte
	err = r.Tx().QueryRowContext(r.Context(), "SELECT zeroblob(34603008)").Scan(&large)
	if err == nil || !errors.Is(StorageError(r.Context(), err), ErrLimit) {
		t.Fatal("SQLite length budget absent", err)
	}
	var value string
	err = r.Tx().QueryRowContext(r.Context(), `SELECT * FROM private_diagnostic_marker`).Scan(&value)
	classified := StorageError(r.Context(), err)
	if err == nil || !errors.Is(classified, ErrStorage) || strings.Contains(classified.Error(), "private_diagnostic_marker") {
		t.Fatal("SQL diagnostic leaked", classified)
	}
	if err = r.Finish(); err != nil {
		t.Fatal(err)
	}
}
