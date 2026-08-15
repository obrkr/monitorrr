//go:build windows

package agent

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
)

// detachedProcess is CREATE_NEW_PROCESS_GROUP's companion from the Windows
// API. Go's syscall package does not export it, and pulling in x/sys for a
// single constant is not worth a dependency. It stops the teardown helper
// sharing our console, so it survives this process exiting.
const detachedProcess = 0x00000008

// serviceInstalled reports whether the scheduled task exists, which is what
// distinguishes a real installation from someone running the binary by hand.
func serviceInstalled() bool {
	cmd := exec.Command("schtasks", "/query", "/tn", "monitorrr-agent")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run() == nil
}

// spawnUninstaller starts a detached cmd that tears down the installation.
//
// The scheduled task goes first so nothing restarts the agent once it exits.
// Windows then refuses to delete a running image, so the binary cannot go until
// this process has actually gone — hence the wait, and hence the retries: a
// single attempt three seconds later is a guess about how long our own exit
// takes, and losing that race leaves an executable behind for good.
func spawnUninstaller(log *slog.Logger, plan uninstallPlan) error {
	var (
		steps []string
		parts []string
	)

	logPath := plan.UninstallLog()
	appendLog := func(message, path string) string {
		if logPath == "" {
			return "rem no log"
		}
		return fmt.Sprintf(`echo %s %s>> "%s"`, message, path, logPath)
	}

	parts = append(parts, `schtasks /delete /tn "monitorrr-agent" /f >nul 2>&1`)
	steps = append(steps, "delete the scheduled task monitorrr-agent")

	// timeout is the shell's sleep; the redirect keeps it quiet with no console
	// attached, which is the case under a scheduled task.
	parts = append(parts, "timeout /t 3 /nobreak >nul")

	if plan.StatePath != "" {
		parts = append(parts, fmt.Sprintf(`del /f /q "%s" >nul 2>&1`, plan.StatePath))
		steps = append(steps, "remove "+plan.StatePath)
	}

	// Each candidate is attempted repeatedly: the file cannot be deleted until
	// our process has exited, and how long that takes is not ours to know.
	for _, binary := range plan.BinaryPaths() {
		parts = append(parts, fmt.Sprintf(
			`for /l %%%%i in (1,1,10) do @(if exist "%s" (del /f /q "%s" >nul 2>&1 & timeout /t 2 /nobreak >nul))`,
			binary, binary))
		parts = append(parts, fmt.Sprintf(`if exist "%s" (%s) else (%s)`,
			binary, appendLog("FAILED to remove", binary), appendLog("removed", binary)))
		steps = append(steps, "remove "+binary)
	}

	if plan.RemoveStateDir {
		// rmdir without /s: only removes the directory when it is empty, which
		// it will not be — the retirement marker and this log both live there.
		parts = append(parts, fmt.Sprintf(`rmdir "%s" >nul 2>&1`, plan.StateDir))
		steps = append(steps, "remove "+plan.StateDir+" (if empty; the retirement marker is kept)")
	}

	logUninstallPlan(log, steps)

	cmd := exec.Command("cmd", "/c", strings.Join(parts, " & "))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start uninstaller: %w", err)
	}
	// Do not Wait: the helper deliberately outlives us.
	return nil
}
