//go:build !windows

package agent

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

const (
	systemdUnit  = "/etc/systemd/system/monitorrr-agent.service"
	launchdPlist = "/Library/LaunchDaemons/io.monitorrr.agent.plist"
)

// spawnUninstaller starts a detached shell that tears down the installation a
// few seconds after this process exits.
func spawnUninstaller(log *slog.Logger, exePath string, removeBinary bool, stateDir string) error {
	var steps []string

	// A short delay lets this process exit first, so the service manager sees a
	// clean shutdown rather than killing us partway through our own removal.
	script := []string{"sleep 3"}

	switch runtime.GOOS {
	case "darwin":
		if _, err := os.Stat(launchdPlist); err == nil {
			script = append(script,
				fmt.Sprintf("launchctl unload -w %q 2>/dev/null || true", launchdPlist),
				fmt.Sprintf("rm -f %q", launchdPlist))
			steps = append(steps, "unload launchd daemon", "remove "+launchdPlist)
		}
	default:
		if _, err := os.Stat(systemdUnit); err == nil {
			script = append(script,
				"systemctl disable --now monitorrr-agent 2>/dev/null || true",
				fmt.Sprintf("rm -f %q", systemdUnit),
				"systemctl daemon-reload 2>/dev/null || true")
			steps = append(steps, "disable systemd service", "remove "+systemdUnit)
		}
	}

	if stateDir != "" && stateDir != "/" && stateDir != "." {
		script = append(script, fmt.Sprintf("rm -rf %q", stateDir))
		steps = append(steps, "remove "+stateDir)
	}
	if removeBinary {
		script = append(script, fmt.Sprintf("rm -f %q", exePath))
		steps = append(steps, "remove "+exePath)
	}

	logUninstallPlan(log, steps)

	cmd := exec.Command("/bin/sh", "-c", strings.Join(script, "; "))
	// Setsid detaches the helper from our process group, so the service manager
	// killing our group on shutdown does not take the uninstaller with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start uninstaller: %w", err)
	}
	// Do not Wait: the helper deliberately outlives us.
	return nil
}
