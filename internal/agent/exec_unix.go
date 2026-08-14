//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the script in its own process group so the whole tree
// can be signalled as a unit.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the script and every process it started. A negative
// PID addresses the process group.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// The group may already be gone, or setpgid may not have taken effect;
		// fall back to killing just the process we know about.
		return cmd.Process.Kill()
	}
	return nil
}
