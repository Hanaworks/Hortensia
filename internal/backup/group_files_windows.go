package backup

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
)

func groupSafeLeaf(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, " ")
}
func groupOpenedPath(path string, f *os.File, info os.FileInfo, dir bool) error {
	if !dir {
		var data windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &data) != nil || data.NumberOfLinks != 1 {
			return ErrInput
		}
	}
	buffer := make([]uint16, 32768)
	n, e := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buffer[0], uint32(len(buffer)), 0)
	if e != nil || n == 0 || n >= uint32(len(buffer)) {
		return ErrInput
	}
	canonical := strings.TrimPrefix(windows.UTF16ToString(buffer[:n]), `\\?\`)
	if !strings.EqualFold(filepath.Clean(path), filepath.Clean(canonical)) {
		return ErrInput
	}
	return nil
}
func groupRenameNoReplace(oldPath, newPath string) error {
	old, e := windows.UTF16PtrFromString(oldPath)
	if e != nil {
		return ErrDestination
	}
	next, e := windows.UTF16PtrFromString(newPath)
	if e != nil {
		return ErrDestination
	}
	if windows.MoveFileEx(old, next, windows.MOVEFILE_WRITE_THROUGH) != nil {
		return ErrDestination
	}
	return nil
}
