//go:build linux

package workspace

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path"
	"strings"
)

type secureFS struct{ fd int }

func openSecure(root string) (*secureFS, error) {
	if !strings.HasPrefix(root, "/") {
		return nil, ErrInput
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if p == "" {
			continue
		}
		n, e := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = n
	}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 {
		unix.Close(fd)
		return nil, ErrDrift
	}
	return &secureFS{fd}, nil
}
func (f *secureFS) Close() error { return unix.Close(f.fd) }
func (f *secureFS) dir(name string, create bool) (int, error) {
	fd, err := unix.Dup(f.fd)
	if err != nil {
		return -1, err
	}
	if name == "." || name == "" {
		return fd, nil
	}
	if !SafePath(name) {
		unix.Close(fd)
		return -1, ErrInput
	}
	for _, p := range strings.Split(name, "/") {
		if create {
			e := unix.Mkdirat(fd, p, 0700)
			if e != nil && !errors.Is(e, unix.EEXIST) {
				unix.Close(fd)
				return -1, e
			}
			if e == nil {
				if e = unix.Fsync(fd); e != nil {
					unix.Close(fd)
					return -1, e
				}
			}
		}
		n, e := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		var info unix.Stat_t
		if e = unix.Fstat(n, &info); e != nil || info.Uid != uint32(os.Geteuid()) || info.Mode&0077 != 0 {
			unix.Close(n)
			return -1, ErrDrift
		}
		fd = n
	}
	return fd, nil
}
func (f *secureFS) MkdirAll(name string) error {
	fd, err := f.dir(name, true)
	if err == nil {
		err = unix.Fsync(fd)
		unix.Close(fd)
	}
	return err
}
func (f *secureFS) Exists(name string) (bool, error) {
	fd, err := f.dir(path.Dir(name), false)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	err = unix.Fstatat(fd, path.Base(name), &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, ErrDrift
	}
	return true, nil
}
func (f *secureFS) ReadFile(name string) ([]byte, error) {
	fd, err := f.dir(path.Dir(name), false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	n, err := unix.Openat(fd, path.Base(name), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(n), name)
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return nil, ErrDrift
	}
	var info unix.Stat_t
	if unix.Fstat(n, &info) != nil || info.Uid != uint32(os.Geteuid()) || info.Mode&0077 != 0 {
		return nil, ErrDrift
	}
	return io.ReadAll(io.LimitReader(file, (4<<20)+1))
}
func (f *secureFS) WriteExclusive(name string, b []byte) error {
	fd, err := f.dir(path.Dir(name), false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	n, err := unix.Openat(fd, path.Base(name), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(n), name)
	_, err = file.Write(b)
	if err == nil {
		err = file.Sync()
	}
	e := file.Close()
	if err == nil {
		err = e
	}
	if err == nil {
		err = unix.Fsync(fd)
	}
	return err
}
func (f *secureFS) Rename(a, b string) error {
	af, err := f.dir(path.Dir(a), false)
	if err != nil {
		return err
	}
	defer unix.Close(af)
	bf, err := f.dir(path.Dir(b), false)
	if err != nil {
		return err
	}
	defer unix.Close(bf)
	if err = unix.Renameat2(af, path.Base(a), bf, path.Base(b), unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err = unix.Fsync(af); err != nil {
		return err
	}
	return unix.Fsync(bf)
}
func (f *secureFS) Files(root string) ([]string, error) {
	var result []string
	var walk func(string, string) error
	walk = func(dir, rel string) error {
		fd, err := f.dir(dir, false)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), dir)
		entries, err := file.ReadDir(-1)
		file.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			n := path.Join(rel, e.Name())
			if e.IsDir() {
				result = append(result, n+"/")
				if err = walk(path.Join(dir, e.Name()), n); err != nil {
					return err
				}
			} else {
				if !e.Type().IsRegular() {
					return ErrDrift
				}
				result = append(result, n)
			}
		}
		return nil
	}
	err := walk(root, "")
	return result, err
}
