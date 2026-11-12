//go:build !linux

package executor

import (
	"os/exec"
)

func configureProcess(cmd *exec.Cmd, mutation bool) error {
	if mutation {
		return ErrUnsupported
	}
	return nil
}
func terminateProcess(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
func wasSignaled(cmd *exec.Cmd) bool { return false }
