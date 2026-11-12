package localstate

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func IsReparse(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return info.Mode()&os.ModeSymlink != 0 || ok && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// Windows validation does not assert Unix mode bits establish an ACL. The
// supported installation target is Linux; Windows is a local test platform.
func PrivateMode(info os.FileInfo, dir bool) bool { return true }
func lockFile(file *os.File) error {
	var o windows.Overlapped
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &o)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	if err != nil {
		return ErrFiles
	}
	return nil
}
func unlockFile(file *os.File) error {
	var o windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &o)
}

// Windows tests verify file FlushFileBuffers only. Parent-directory durability
// is a separate Linux acceptance item, never inferred from this return value.
func SyncDirectory(path string) error { return nil }
