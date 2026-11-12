package service

import (
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Check all existing components before accessing a configured private path.
// The leaf directory must also be private, so another local user cannot race
// a component replacement while the service writes identity material.
func rejectLinks(path string) error {
	if !filepath.IsAbs(path) {
		return ErrFiles
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return ErrFiles
		}
		if err == nil && isReparse(info) {
			return ErrFiles
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if err := rejectLinks(path); err != nil {
		return err
	}
	var missing []string
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return ErrFiles
		}
		missing = append(missing, p)
		if filepath.Dir(p) == p {
			return ErrFiles
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return ErrFiles
	}
	for _, p := range missing {
		if syncDirectory(p) != nil || syncDirectory(filepath.Dir(p)) != nil {
			return ErrFiles
		}
	}
	return checkPrivateDir(path)
}

func checkPrivateDir(path string) error {
	if err := rejectLinks(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || !privateMode(info, true) {
		return ErrFiles
	}
	return nil
}

func writeExclusive(path string, raw []byte) error {
	if err := rejectLinks(path); err != nil {
		return err
	}
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return ErrInitialized
	}
	if err != nil {
		return ErrFiles
	}
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrFiles
	}
	return syncDirectory(filepath.Dir(path))
}

func readPrivateFile(path string, max int64) ([]byte, error) {
	if err := rejectLinks(path); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrFiles
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !privateMode(info, false) || info.Size() > max {
		return nil, ErrFiles
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, ErrFiles
	}
	return raw, nil
}

type fileLock struct {
	file *os.File
	once sync.Once
	err  error
}

func acquireFileLock(path string) (*fileLock, error) {
	if err := rejectLinks(path); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrFiles
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !privateMode(info, false) {
		f.Close()
		return nil, ErrFiles
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &fileLock{file: f}, nil
}

func (l *fileLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		unlockErr := unlockFile(l.file)
		closeErr := l.file.Close()
		if unlockErr != nil || closeErr != nil {
			l.err = ErrFiles
		}
	})
	return l.err
}
