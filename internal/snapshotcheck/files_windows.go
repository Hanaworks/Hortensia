package snapshotcheck

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func singleLink(path string, info os.FileInfo) (ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() {
		if f.Close() != nil {
			ok = false
		}
	}()
	var data windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &data) != nil || data.NumberOfLinks != 1 {
		return false
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return false
	}
	// A Win32 alias can open the main file while a string-appended sidecar
	// names a different file. Reject aliases before trusting sidecar absence or
	// giving the path to SQLite immutable mode; case differences are harmless.
	buffer := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buffer[0], uint32(len(buffer)), 0)
	if err != nil || n == 0 || n >= uint32(len(buffer)) {
		return false
	}
	canonical := strings.TrimPrefix(windows.UTF16ToString(buffer[:n]), `\\?\`)
	return strings.EqualFold(filepath.Clean(path), filepath.Clean(canonical))
}
