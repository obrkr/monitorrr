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

// Where the installer registers the agent. Variables rather than constants for
// the same reason installPath is: a deployment that puts them elsewhere can set
// them at build time, and tests can exercise teardown without a real service.
var (
	systemdUnit  = "/etc/systemd/system/monitorrr-agent.service"
	launchdPlist = "/Library/LaunchDaemons/io.monitorrr.agent.plist"
)

// serviceInstalled reports whether a service manager knows about this agent,
// which is what distinguishes a real installation from someone running the
// binary by hand.
func serviceInstalled() bool {
	path := systemdUnit
	if runtime.GOOS == "darwin" {
		path = launchdPlist
	}
	_, err := os.Stat(path)
	return err == nil
}

// spawnUninstaller starts a detached helper that tears down the installation.
//
// Ordering is the whole design here, and it is not obvious.
//
// Stopping the service can kill the helper doing the stopping. On macOS,
// launchd kills the job's process group, which setsid escapes — which is why
// this worked there. systemd does not work that way: it tracks a service by
// cgroup and, with the default KillMode=control-group, kills every process in
// it. The helper is forked from the agent, so it starts life inside that
// cgroup, and setsid says nothing about cgroup membership. Stopping the unit
// therefore killed the helper mid-script and left the binary behind.
//
// Two things address it. The teardown runs inside a transient systemd unit
// where systemd-run is available, giving it a cgroup of its own; and the steps
// are ordered so stopping the service comes last, after everything else has
// already been removed. Either alone fixes it. Both together mean a host
// without systemd-run still ends up clean.
func spawnUninstaller(log *slog.Logger, plan uninstallPlan) error {
	var (
		steps  []string
		script []string
	)

	// Let the agent exit before its files go away.
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

	// Service teardown last, because it is the step that can kill this helper.
	switch runtime.GOOS {
	case "darwin":
		if _, err := os.Stat(launchdPlist); err == nil {
			script = append(script,
				fmt.Sprintf("rm -f %q", launchdPlist),
				fmt.Sprintf("launchctl unload -w %q 2>/dev/null || true", launchdPlist))
			steps = append(steps, "remove "+launchdPlist, "unload the launchd daemon")
		}
	default:
		if _, err := os.Stat(systemdUnit); err == nil {
			script = append(script,
				// disable without --now removes the enablement symlinks without
				// stopping anything, so it cannot kill us.
				"systemctl disable monitorrr-agent 2>/dev/null || true",
				fmt.Sprintf("rm -f %q", systemdUnit),
				"systemctl daemon-reload 2>/dev/null || true",
				// The one step that may take this helper with it — and by now
				// there is nothing left for it to interrupt.
				"systemctl stop monitorrr-agent 2>/dev/null || true")
			steps = append(steps, "remove "+systemdUnit, "disable and stop the systemd service")
		}
	}

	logUninstallPlan(log, steps)

	cmd := uninstallCommand(log, strings.Join(script, "; "))
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start uninstaller: %w", err)
	}
	// A plain shell deliberately outlives us and is left alone. systemd-run is
	// different: it returns as soon as the transient unit is queued, so reaping
	// it leaves no zombie behind.
	if filepathBase(cmd.Path) == "systemd-run" {
		go cmd.Wait()
	}
	return nil
}

// uninstallCommand builds the process that runs the teardown.
//
// systemd-run puts it in a transient unit with its own cgroup, so stopping the
// agent's service cannot kill it. Where that is unavailable — no systemd, or
// not running as root — a setsid shell is used instead, and the step ordering
// above is what keeps that case correct.
func uninstallCommand(log *slog.Logger, script string) *exec.Cmd {
	if runtime.GOOS != "darwin" && os.Geteuid() == 0 {
		if path, err := exec.LookPath("systemd-run"); err == nil {
			log.Info("running teardown in a transient systemd unit",
				"reason", "a helper inside our own cgroup is killed when the service stops")
			return exec.Command(path,
				"--collect",
				"--unit=monitorrr-uninstall",
				"--description=monitorrr agent uninstall",
				"/bin/sh", "-c", script)
		}
	}

	cmd := exec.Command("/bin/sh", "-c", script)
	// Setsid detaches from our process group, which is what launchd kills.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
