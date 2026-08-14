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

	removeBinary := exe != "" && exe == canonicalInstallPath()
	if exe != "" && !removeBinary {
		a.log.Warn("not removing the binary: it is not at the standard install path",
			"path", exe, "expected", canonicalInstallPath())
	}

	// Remove our identity synchronously. If anything below fails, the leftover
	// binary is inert rather than re-enrolling this machine on next boot.
	if err := os.Remove(a.cfg.StatePath); err != nil && !os.IsNotExist(err) {
		a.log.Error("could not remove state file", "path", a.cfg.StatePath, "error", err)
	}

	if err := spawnUninstaller(a.log, exe, removeBinary, filepath.Dir(a.cfg.StatePath)); err != nil {
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
