package service

import (
	"docker-manager/internal/localstate"
	"os"
)

type fileLock = localstate.Lock

func rejectLinks(p string) error                        { return localstate.RejectLinks(p) }
func ensurePrivateDir(p string) error                   { return localstate.EnsurePrivateDir(p) }
func checkPrivateDir(p string) error                    { return localstate.CheckPrivateDir(p) }
func writeExclusive(p string, b []byte) error           { return localstate.WriteExclusive(p, b) }
func readPrivateFile(p string, n int64) ([]byte, error) { return localstate.ReadPrivateFile(p, n) }
func acquireFileLock(p string) (*fileLock, error)       { return localstate.AcquireLock(p) }
func syncDirectory(p string) error                      { return localstate.SyncDirectory(p) }
func isReparse(i os.FileInfo) bool                      { return localstate.IsReparse(i) }
func privateMode(i os.FileInfo, d bool) bool            { return localstate.PrivateMode(i, d) }
