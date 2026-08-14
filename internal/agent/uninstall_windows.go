//go:build windows

package agent

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
)

// spawnUninstaller starts a detached cmd that tears down the installation after
// this process exits. Windows will not delete a running executable, so the
// delay is load-bearing rather than cosmetic.
func spawnUninstaller(log *slog.Logger, exePath string, removeBinary bool, stateDir string) error {
	var steps []string

	// timeout is the shell-friendly sleep; the redirect keeps it quiet when
	// there is no console attached, which is the case under a scheduled task.
	parts := []string{"timeout /t 3 /nobreak >nul"}

	parts = append(parts, `schtasks /delete /tn "monitorrr-agent" /f >nul 2>&1`)
	steps = append(steps, "delete scheduled task monitorrr-agent")

	if stateDir != "" {
		parts = append(parts, fmt.Sprintf(`rmdir /s /q "%s" >nul 2>&1`, stateDir))
		steps = append(steps, "remove "+stateDir)
	}
	if removeBinary {
		parts = append(parts, fmt.Sprintf(`del /f /q "%s" >nul 2>&1`, exePath))
		steps = append(steps, "remove "+exePath)
	}

	logUninstallPlan(log, steps)

	cmd := exec.Command("cmd", "/c", strings.Join(parts, " & "))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start uninstaller: %w", err)
	}
	// Do not Wait: the helper deliberately outlives us.
	return nil
}
