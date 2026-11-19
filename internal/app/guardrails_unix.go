//go:build !windows

package app

import (
	"os/exec"
	"syscall"
)

// configureGuardrailsChild puts the sidecar in its own process group so a stop
// reaches any worker the Python process spawned, not only the parent.
func configureGuardrailsChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateGuardrailsChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func killGuardrailsChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
