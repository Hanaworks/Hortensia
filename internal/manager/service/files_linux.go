package service

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func isReparse(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
func privateMode(info os.FileInfo, dir bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	if dir {
		return info.Mode().Perm() == 0700
	}
	return info.Mode().Perm()&0177 == 0 && info.Mode().Perm()&0400 != 0
}
func lockFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrLocked
	}
	if err != nil {
		return ErrFiles
	}
	return nil
}
func unlockFile(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return ErrFiles
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrFiles
	}
	return nil
}
