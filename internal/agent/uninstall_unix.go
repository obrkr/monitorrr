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

// spawnUninstaller starts a detached shell that tears down the installation.
//
// Step order matters. The service is stopped first, with no delay, because
// until it is gone the supervisor will restart the agent the moment we exit.
// Only then do we wait and remove files.
func spawnUninstaller(log *slog.Logger, plan uninstallPlan) error {
	var (
		steps  []string
		script []string
	)

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

	// Give the agent a moment to exit before its files go away.
	script = append(script, "sleep 2")

	if plan.StatePath != "" {
		script = append(script, fmt.Sprintf("rm -f %q", plan.StatePath))
		steps = append(steps, "remove "+plan.StatePath)
	}
	if plan.RemoveStateDir {
		// rmdir, not rm -rf: if anything unexpected is in there, leaving it is
		// far better than recursively deleting a directory we guessed at. In
		// practice this leaves the directory holding just the retirement
		// marker, which is intended — the marker must outlive teardown.
		script = append(script, fmt.Sprintf("rmdir %q 2>/dev/null || true", plan.StateDir))
		steps = append(steps, "remove "+plan.StateDir+" (if empty; the retirement marker is kept)")
	}
	if plan.RemoveBinary {
		script = append(script, fmt.Sprintf("rm -f %q", plan.ExePath))
		steps = append(steps, "remove "+plan.ExePath)
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
