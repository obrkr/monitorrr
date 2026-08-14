//go:build windows

package agent

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcessGroup gives the script its own process group, so a Ctrl-C in the
// agent's console does not also interrupt a running job.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup terminates the script and its descendants. Windows has no
// process-group signal equivalent, so this shells out to taskkill /T, which
// walks the tree — the alternative is a Job Object and a lot more code.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
