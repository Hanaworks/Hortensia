//go:build !linux && !windows

package snapshotcheck

import "os"

func singleLink(string, os.FileInfo) bool { return false }
