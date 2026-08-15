package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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

// planUninstall works out which paths belong to a real installation.
//
// Both flags are deliberately conservative. The state *file* is always ours to
// delete, but its directory is only ours when it is the canonical per-OS
// location: running with -state ./dist/agent-state.json must never lead to
// ./dist being removed, and that directory holds the server binary.
func planUninstall(exePath, statePath string) uninstallPlan {
	p := uninstallPlan{ExePath: exePath, StatePath: statePath}
	if statePath != "" {
		p.StateDir = filepath.Dir(statePath)
		p.RemoveStateDir = p.StateDir == filepath.Dir(DefaultStatePath())
	}
	p.RemoveBinary = exePath != "" && exePath == canonicalInstallPath()
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

	exe, err := os.Executable()
	if err != nil {
		a.log.Error("could not determine own path; the binary will be left in place", "error", err)
		exe = ""
	}
	if exe != "" {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}

	plan := planUninstall(exe, a.cfg.StatePath)
	if exe != "" && !plan.RemoveBinary {
		a.log.Warn("not removing the binary: it is not at the standard install path",
			"path", exe, "expected", canonicalInstallPath())
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
