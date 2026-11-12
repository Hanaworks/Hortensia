package backup

import (
	"os"

	"docker-manager/internal/localstate"
	"golang.org/x/sys/windows"
)

func singleRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || localstate.IsReparse(info) {
		return ErrInput
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrInput
	}
	defer file.Close()
	var data windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &data) != nil || data.NumberOfLinks != 1 {
		return ErrInput
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return ErrInput
	}
	return nil
}
