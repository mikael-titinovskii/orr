//go:build windows

package app

import (
	"os/exec"
	"strconv"
	"syscall"
)

// configureGuardrailsChild gives the sidecar its own process group, which is
// what lets taskkill /T reach the interpreter's children.
func configureGuardrailsChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func terminateGuardrailsChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}

func killGuardrailsChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
