package snapshotcheck

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPathAliasesCannotHideCommittedWAL(t *testing.T) {
	p, db := testDB(t, false)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA journal_mode=WAL; UPDATE control SET boot='committed-in-real-wal'"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(p + "-wal"); err != nil || info.Size() <= 32 {
		t.Fatal("fixture did not retain committed WAL", err)
	}
	for _, alias := range []string{p + ".", p + " ", filepath.Join(filepath.Dir(p)+".", filepath.Base(p)), filepath.Join(filepath.Dir(p)+" ", filepath.Base(p))} {
		t.Run(alias, func(t *testing.T) {
			r, err := Open(context.Background(), alias)
			if !errors.Is(err, ErrInput) {
				if r != nil {
					defer r.Close()
					var boot string
					queryErr := r.Tx().QueryRowContext(r.Context(), "SELECT boot FROM control").Scan(&boot)
					t.Fatalf("alias accepted live WAL: open=%v boot=%q query=%v finish=%v", err, boot, queryErr, r.Finish())
				}
				t.Fatalf("alias not rejected: %v", err)
			}
		})
	}
}

func TestWindowsShortFileNamesCannotHideCommittedWAL(t *testing.T) {
	p, db := testDB(t, false)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(filepath.Dir(p), "a-closed-snapshot-with-a-long-database-name.sqlite")
	if err := os.Rename(p, long); err != nil {
		t.Fatal(err)
	}
	pointer, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	n, err := windows.GetShortPathName(pointer, &buffer[0], uint32(len(buffer)))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 || n >= uint32(len(buffer)) {
		t.Fatal("unexpected short-path size")
	}
	short := windows.UTF16ToString(buffer[:n])
	if strings.EqualFold(filepath.Base(short), filepath.Base(long)) {
		t.Skip("filesystem has no distinct short filename")
	}
	db, err = sql.Open("sqlite", long)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA journal_mode=WAL; UPDATE control SET boot='committed-in-real-wal'"); err != nil {
		t.Fatal(err)
	}
	if r, err := Open(context.Background(), short); !errors.Is(err, ErrInput) {
		if r != nil {
			defer r.Close()
			var boot string
			queryErr := r.Tx().QueryRowContext(r.Context(), "SELECT boot FROM control").Scan(&boot)
			t.Fatalf("short name hid live WAL: open=%v boot=%q query=%v finish=%v", err, boot, queryErr, r.Finish())
		}
		t.Fatal(err)
	}
}
