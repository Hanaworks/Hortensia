package snapshotcheck

import (
	"os"
	"syscall"
)

func singleLink(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
