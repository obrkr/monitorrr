//go:build windows

package agent

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
)

// serviceInstalled reports whether the scheduled task exists, which is what
// distinguishes a real installation from someone running the binary by hand.
func serviceInstalled() bool {
	cmd := exec.Command("schtasks", "/query", "/tn", "monitorrr-agent")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run() == nil
}

// spawnUninstaller starts a detached cmd that tears down the installation.
//
// The scheduled task is deleted first so nothing restarts the agent once it
// exits. Windows will not delete a running executable, so the wait before
// removing files is load-bearing rather than cosmetic.
func spawnUninstaller(log *slog.Logger, plan uninstallPlan) error {
	var (
		steps []string
		parts []string
	)

	parts = append(parts, `schtasks /delete /tn "monitorrr-agent" /f >nul 2>&1`)
	steps = append(steps, "delete scheduled task monitorrr-agent")

	// timeout is the shell-friendly sleep; the redirect keeps it quiet when
	// there is no console attached, which is the case under a scheduled task.
	parts = append(parts, "timeout /t 3 /nobreak >nul")

	if plan.StatePath != "" {
		parts = append(parts, fmt.Sprintf(`del /f /q "%s" >nul 2>&1`, plan.StatePath))
		steps = append(steps, "remove "+plan.StatePath)
	}
	if plan.RemoveStateDir {
		// rmdir without /s: only removes the directory when it is empty.
		parts = append(parts, fmt.Sprintf(`rmdir "%s" >nul 2>&1`, plan.StateDir))
		steps = append(steps, "remove "+plan.StateDir+" (if empty)")
	}
	if plan.RemoveBinary {
		parts = append(parts, fmt.Sprintf(`del /f /q "%s" >nul 2>&1`, plan.ExePath))
		steps = append(steps, "remove "+plan.ExePath)
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
