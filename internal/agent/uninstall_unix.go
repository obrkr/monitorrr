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

	logPath := plan.UninstallLog()

	// Let the agent exit before its files go away.
	script = append(script, "sleep 2")
	if logPath != "" {
		script = append(script,
			"echo \"$(date -u +%Y-%m-%dT%H:%M:%SZ) monitorrr uninstall starting\" >> "+shellQuote(logPath)+" 2>/dev/null || true")
	}

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
	// Removing the binary is the part that must not fail quietly, so each
	// candidate is retried and the outcome recorded.
	for _, binary := range plan.BinaryPaths() {
		script = append(script, removeAndVerify(binary, logPath))
		steps = append(steps, "remove "+binary)
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
	joined := strings.Join(script, "; ")

	// Try the transient unit first, and check that it actually started. It
	// returns as soon as the unit is queued, so waiting costs nothing — and
	// not waiting would mean a failure here (a leftover unit of the same name
	// is enough) silently skipped the entire teardown with no fallback.
	if runner := systemdRunCommand(joined); runner != nil {
		out, err := runner.CombinedOutput()
		if err == nil {
			log.Info("teardown running in a transient systemd unit",
				"reason", "a helper in our own cgroup is killed when the service stops")
			return nil
		}
		log.Warn("could not start the teardown as a transient unit, falling back to a detached shell",
			"error", err, "output", strings.TrimSpace(string(out)))
	}

	cmd := exec.Command("/bin/sh", "-c", joined)
	// Setsid detaches from our process group, which is what launchd kills.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start uninstaller: %w", err)
	}
	// Do not Wait: the shell deliberately outlives us.
	return nil
}

// removeAndVerify deletes a path, retries once if it is still there, and
// records the outcome.
//
// A plain "rm -f" reports success whether or not the file existed and whether
// or not it went, which is precisely the failure mode that let a retired agent
// keep its binary.
//
// Built by concatenation rather than a nested format string: an earlier version
// passed one format string through two Sprintf rounds, and date's own %-escapes
// came out mangled on the second pass, breaking the whole script.
func removeAndVerify(path, logPath string) string {
	quoted := shellQuote(path)
	rm := "rm -f " + quoted + " 2>/dev/null || true"
	retry := "if [ -e " + quoted + " ]; then sleep 3; rm -f " + quoted + " 2>/dev/null || true; fi"

	if logPath == "" {
		return rm + "; " + retry
	}

	stamp := "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	quotedLog := shellQuote(logPath)
	note := func(outcome string) string {
		return "echo \"" + stamp + " " + outcome + " " + path + "\" >> " + quotedLog + " 2>/dev/null || true"
	}
	return rm + "; " + retry + "; " +
		"if [ -e " + quoted + " ]; then " + note("FAILED to remove") + "; else " + note("removed") + "; fi"
}

// shellQuote wraps a path in single quotes for /bin/sh, escaping any single
// quotes within it.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// systemdRunCommand builds the transient-unit invocation, or nil where that is
// not available: no systemd, or not running as root.
//
// The unit name carries the process id so a leftover unit from an earlier
// attempt cannot block this one — systemd refuses to reuse a name that still
// exists, and that failure would otherwise cost the whole teardown.
func systemdRunCommand(script string) *exec.Cmd {
	if runtime.GOOS == "darwin" || os.Geteuid() != 0 {
		return nil
	}
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil
	}
	return exec.Command(path,
		"--collect",
		fmt.Sprintf("--unit=monitorrr-uninstall-%d", os.Getpid()),
		"--description=monitorrr agent uninstall",
		"/bin/sh", "-c", script)
}
