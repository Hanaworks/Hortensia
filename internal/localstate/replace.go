package localstate

import (
	"errors"
	"os"
	"path/filepath"
)

var ErrFiles = errors.New("private files invalid")
var ErrLocked = errors.New("state directory already locked")
var ErrInitialized = errors.New("initialization materials already exist")

// CheckPrivateFile validates metadata without loading database-sized material.
func CheckPrivateFile(path string) error {
	if RejectLinks(path) != nil || CheckPrivateDir(filepath.Dir(path)) != nil {
		return ErrFiles
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !PrivateMode(info, false) {
		return ErrFiles
	}
	return nil
}

// Replace publishes bytes only in a checked private directory. The original
// destination must be a regular private file; rename never follows a link.
func Replace(path string, raw []byte) error {
	if RejectLinks(path) != nil || CheckPrivateDir(filepath.Dir(path)) != nil {
		return ErrFiles
	}
	if _, err := ReadPrivateFile(path, 1<<20); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".identity-*")
	if err != nil {
		return ErrFiles
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if f.Chmod(0600) != nil {
		f.Close()
		return ErrFiles
	}
	_, w := f.Write(raw)
	s := f.Sync()
	c := f.Close()
	if w != nil || s != nil || c != nil {
		return ErrFiles
	}
	if RejectLinks(path) != nil {
		return ErrFiles
	}
	if os.Rename(tmp, path) != nil {
		return ErrFiles
	}
	return SyncDirectory(filepath.Dir(path))
}
