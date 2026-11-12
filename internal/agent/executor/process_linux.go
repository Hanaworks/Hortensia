//go:build linux

package executor

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd, mutation bool) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}
func terminateProcess(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}
func wasSignaled(cmd *exec.Cmd) bool {
	if cmd.ProcessState == nil {
		return false
	}
	s, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ok && s.Signaled()
}
