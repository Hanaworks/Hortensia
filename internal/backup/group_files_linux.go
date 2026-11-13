package backup

import (
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func groupSafeLeaf(name string) bool { return name != "" && name != "." && name != ".." }
func groupOpenedPath(_ string, _ *os.File, info os.FileInfo, dir bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !dir && stat.Nlink != 1 {
		return ErrInput
	}
	return nil
}
func groupRenameNoReplace(oldPath, newPath string) error {
	if unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE) != nil {
		return ErrDestination
	}
	return nil
}
