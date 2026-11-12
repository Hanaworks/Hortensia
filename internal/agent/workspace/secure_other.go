//go:build !linux

package workspace

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// This path is for controlled tests only; Linux uses descriptor-relative IO.
type secureFS struct{ root string }

func openSecure(root string) (*secureFS, error) {
	if !filepath.IsAbs(root) {
		return nil, ErrInput
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrDrift
	}
	return &secureFS{root}, nil
}
func (f *secureFS) Close() error { return nil }
func (f *secureFS) resolve(name string) (string, error) {
	if name != "." && !SafePath(name) {
		return "", ErrInput
	}
	p := f.root
	for _, part := range strings.Split(name, "/") {
		if part == "." {
			continue
		}
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", ErrDrift
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	return p, nil
}
func (f *secureFS) MkdirAll(name string) error {
	p, err := f.resolve(name)
	if err != nil {
		return err
	}
	return os.MkdirAll(p, 0700)
}
func (f *secureFS) Exists(name string) (bool, error) {
	p, err := f.resolve(name)
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
func (f *secureFS) ReadFile(name string) ([]byte, error) {
	p, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return nil, ErrDrift
	}
	return io.ReadAll(io.LimitReader(file, (4<<20)+1))
}
func (f *secureFS) WriteExclusive(name string, b []byte) error {
	p, err := f.resolve(name)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(b)
	if err == nil {
		err = file.Sync()
	}
	e := file.Close()
	if err == nil {
		err = e
	}
	return err
}
func (f *secureFS) Rename(a, b string) error {
	src, err := f.resolve(a)
	if err != nil {
		return err
	}
	dst, err := f.resolve(b)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(dst); !os.IsNotExist(err) {
		return ErrDrift
	}
	return os.Rename(src, dst)
}
func (f *secureFS) Files(root string) ([]string, error) {
	p, err := f.resolve(root)
	if err != nil {
		return nil, err
	}
	var names []string
	err = filepath.WalkDir(p, func(n string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n == p {
			return nil
		}
		if e.Type()&os.ModeSymlink != 0 {
			return ErrDrift
		}
		rel, _ := filepath.Rel(p, n)
		rel = filepath.ToSlash(rel)
		if e.IsDir() {
			rel += "/"
		} else if !e.Type().IsRegular() {
			return ErrDrift
		}
		names = append(names, rel)
		return nil
	})
	return names, err
}
