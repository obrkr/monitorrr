package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// canonicalInstallPath is where the installer puts the binary. Self-uninstall
// only removes the executable when it is running from here — otherwise someone
// testing a build from a working directory would have it deleted underneath
// them, which is a nasty surprise for a very small convenience.
func canonicalInstallPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Program Files\monitorrr\monitorrr-agent.exe`
	}
	return "/usr/local/bin/monitorrr-agent"
}

// uninstallPlan decides what teardown may safely remove.
type uninstallPlan struct {
	ExePath        string
	RemoveBinary   bool
	StatePath      string
	StateDir       string
	RemoveStateDir bool
}

// BinaryPaths is every path the agent's binary may be at, most specific first.
//
// Two rather than one because they can differ, and removing the wrong one is
// indistinguishable from removing none: the path a process was started from can
// be stale after a self-update, while the install path is where the file
// actually lives. Trying both costs nothing and is the difference between
// retirement leaving an executable behind or not.
func (p uninstallPlan) BinaryPaths() []string {
	if !p.RemoveBinary {
		return nil
	}
	var out []string
	for _, candidate := range []string{p.ExePath, installPath} {
		if candidate == "" {
			continue
		}
		seen := false
		for _, existing := range out {
			if existing == candidate {
				seen = true
			}
		}
		if !seen {
			out = append(out, candidate)
		}
	}
	return out
}

// UninstallLog is where teardown records what it managed to remove. It sits
// beside the retirement marker so it outlives the state directory, because the
// one thing worse than a failed uninstall is a failed uninstall that left no
// trace of why.
func (p uninstallPlan) UninstallLog() string {
	if p.StateDir == "" {
		return ""
	}
	return filepath.Join(p.StateDir, "uninstall.log")
}

// tombstoneName marks a machine as decommissioned. It sits beside the identity
// file and outlives teardown deliberately.
const tombstoneName = "retired"

// tombstonePath returns the marker location for a given identity file.
func tombstonePath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), tombstoneName)
}

// isTombstoned reports whether this machine has already been retired.
//
// This is the backstop that makes retirement safe under a supervisor. Stopping
// the service is best-effort: the agent may not recognise how it was installed,
// or may lack the privileges to unregister itself. When that happens the
// supervisor restarts it, and without a marker it would enrol as a brand-new
// device — leaving a ghost record that can never be retired because no agent
// remains to receive the instruction.
func (a *Agent) isTombstoned() bool {
	path := tombstonePath(a.cfg.StatePath)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// clearTombstone removes the retirement marker, allowing this machine to enrol
// again. Reinstalling does this too; the flag exists for the cases where the
// installer is not what put the agent there.
func (a *Agent) clearTombstone() error {
	path := tombstonePath(a.cfg.StatePath)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	a.log.Info("retirement marker cleared", "path", path)
	return nil
}

// writeTombstone marks this machine as retired before teardown begins.
func (a *Agent) writeTombstone() {
	path := tombstonePath(a.cfg.StatePath)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		a.log.Error("could not create state directory for the retirement marker", "error", err)
		return
	}
	note := "This machine was retired by monitorrr; the agent will not re-enrol.\n" +
		"Delete this file (or reinstall the agent) to allow enrolment again.\n"
	if err := os.WriteFile(path, []byte(note), 0o644); err != nil {
		a.log.Error("could not write the retirement marker; a supervisor restart may re-enrol this machine",
			"path", path, "error", err)
		return
	}
	a.log.Info("wrote retirement marker", "path", path)
}

// ownExecutable resolves this process's binary path.
//
// Linux reports /proc/self/exe as "/path/to/binary (deleted)" once the file has
// been replaced — which is exactly what a self-update does. os.Executable
// returns that literal string, suffix included, so anything built from it would
// refer to a path that does not exist: teardown would rm a file called
// "monitorrr-agent (deleted)" and remove nothing at all.
func ownExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// removeIfPresent deletes a path, ignoring the case where it was not there.
func removeIfPresent(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return
	}
}

// planUninstall works out which paths belong to a real installation.
//
// The binary is removed when this is a *managed* installation — one a service
// manager knows about — wherever it happens to live, or when it sits at the
// canonical install path. Keying on the service rather than the path alone
// matters because the installer accepts --prefix: an agent installed to
// /opt/monitorrr is every bit as real as one in /usr/local/bin, and leaving its
// binary behind after retirement is exactly the litter this is meant to avoid.
//
// With no service registered, nothing is removed. That is someone running the
// binary by hand — very possibly a build they are in the middle of testing —
// and deleting it under them would be a nasty surprise for no benefit.
//
// The state *file* is always ours to delete, but its directory is only ours
// when it is the canonical per-OS location: running with
// -state ./dist/agent-state.json must never lead to ./dist being removed, and
// that directory holds the server binary.
func planUninstall(exePath, statePath string, managed bool) uninstallPlan {
	p := uninstallPlan{ExePath: exePath, StatePath: statePath}
	if statePath != "" {
		p.StateDir = filepath.Dir(statePath)
		p.RemoveStateDir = p.StateDir == filepath.Dir(DefaultStatePath())
	}
	p.RemoveBinary = exePath != "" && (managed || exePath == canonicalInstallPath())
	return p
}

// retire uninstalls this agent and reports back before exiting.
//
// The teardown itself runs in a detached helper process rather than inline.
// Stopping our own service from inside it is a race we cannot win — systemd
// would kill us mid-cleanup, and Windows will not let a running executable
// delete itself. Handing the work to a process that outlives us sidesteps both.
func (a *Agent) retire(ctx context.Context) {
	a.log.Warn("server requested retirement — uninstalling this agent")

	// Acknowledge first: once teardown starts we may not be able to reach the
	// network, and the server needs to know the instruction was carried out.
	if err := a.post(ctx, "/v1/retire/ack", a.st.AgentID+":"+a.st.AgentToken, struct{}{}, nil); err != nil {
		a.log.Error("could not acknowledge retirement, uninstalling anyway", "error", err)
	}

	exe, err := ownExecutable()
	if err != nil {
		a.log.Error("could not determine own path; the binary will be left in place", "error", err)
		exe = ""
	}

	// A binary replaced by a self-update no longer exists at the path we were
	// started from, but the installation it belongs to still does. Fall back to
	// the install path so retirement still cleans up.
	if exe != "" {
		if _, statErr := os.Stat(exe); statErr != nil {
			a.log.Info("own binary is no longer at the path we started from; using the install path",
				"started_from", exe, "install_path", installPath)
			exe = installPath
		}
	}

	managed := serviceInstalled()
	plan := planUninstall(exe, a.cfg.StatePath, managed)
	if exe != "" && !plan.RemoveBinary {
		a.log.Warn("not removing the binary: no service is registered for it, so this looks like a manual run",
			"path", exe)
	}
	if plan.StateDir != "" && !plan.RemoveStateDir {
		a.log.Info("removing the state file but leaving its directory: not a standard state location",
			"dir", plan.StateDir)
	}

	// Mark the machine before anything is torn down, so a supervisor restart at
	// any point from here on finds the marker and exits instead of enrolling.
	a.writeTombstone()

	// The identity file is deliberately NOT removed here. Deleting it before the
	// service is gone means a supervisor that restarts us brings up an agent
	// with no identity. Teardown removes it after the service has been
	// unloaded, so a restart in that window re-uses the existing identity, is
	// told to retire again, and acknowledges again — which is a no-op.
	if err := spawnUninstaller(a.log, plan); err != nil {
		a.log.Error("could not start the uninstaller", "error", err)
		a.log.Warn("remove the service manually — this agent has stopped but is still installed")
		return
	}
	a.log.Info("uninstaller started; this agent is shutting down")
}

// logUninstallPlan is shared by the platform implementations so the operator
// can see exactly what was scheduled, in the last log lines the agent writes.
func logUninstallPlan(log *slog.Logger, steps []string) {
	for _, step := range steps {
		log.Info("uninstall step scheduled", "action", step)
	}
}
