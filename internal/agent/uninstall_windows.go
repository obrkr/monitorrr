//go:build windows

package agent

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
)

// Windows API constants Go's syscall package does not export, and which are not
// worth a dependency on x/sys for.
const (
	// detachedProcess stops the helper sharing our console, so it survives this
	// process exiting.
	detachedProcess = 0x00000008
	// createBreakawayFromJob leaves the job object we belong to. Task Scheduler
	// runs every task inside one, and tearing the task down kills every process
	// in the job — including a child we started, however detached it is.
	createBreakawayFromJob = 0x01000000
)

// serviceInstalled reports whether the scheduled task exists, which is what
// distinguishes a real installation from someone running the binary by hand.
func serviceInstalled() bool {
	cmd := exec.Command("schtasks", "/query", "/tn", taskName)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run() == nil
}

// spawnUninstaller writes a teardown script and starts it somewhere that
// outlives this process.
//
// Ordering and launch method are both load-bearing, and neither is obvious.
//
// The agent normally runs as a scheduled task, and Task Scheduler puts each
// task instance in a job object. When the task ends, the job is terminated and
// every process in it dies — a child started with DETACHED_PROCESS very much
// included, because detaching from a console says nothing about job membership.
// A helper forked from the agent was therefore killed the instant the agent
// exited, usually before its first command finished, which is why retirement
// left both the binary and the task in place.
//
// Three things address it, in the order they are tried. The teardown runs as
// its own transient scheduled task, which the scheduler starts in a job of its
// own; failing that, it is launched with CREATE_BREAKAWAY_FROM_JOB; failing
// that, plain detached, which is enough when the agent was started by hand. The
// steps are also ordered so deleting the agent's task comes last, after
// everything else is already gone.
func spawnUninstaller(log *slog.Logger, plan uninstallPlan) error {
	script, steps := uninstallScript(plan)
	logUninstallPlan(log, steps)

	path, err := writeUninstallScript(plan, script)
	if err != nil {
		return fmt.Errorf("write uninstaller: %w", err)
	}

	if err := runAsTransientTask(path); err != nil {
		log.Warn("could not run the teardown as a scheduled task, launching it directly", "error", err)
	} else {
		log.Info("teardown running as its own scheduled task",
			"reason", "a helper inside our job object is killed when this task ends")
		return nil
	}

	// Breakaway first, then without: a job that forbids breakaway fails the
	// call outright, and a plain detached process is still correct for an agent
	// that was never registered as a task.
	for _, flags := range []uint32{
		syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess | createBreakawayFromJob,
		syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	} {
		cmd := exec.Command("cmd", "/c", path)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		if err := cmd.Start(); err == nil {
			// Do not Wait: the helper deliberately outlives us.
			return nil
		}
	}
	return fmt.Errorf("could not start the uninstaller at %s", path)
}

// runAsTransientTask registers the teardown as a one-off scheduled task and
// starts it. The scheduler owns the resulting process, so it is in a job of its
// own and nothing that happens to this agent can take it down.
//
// No start date is given and the time is fixed at 23:59 deliberately: /sd wants
// the machine's locale date format, which is not something to guess at, and the
// task is run explicitly rather than waiting for its trigger anyway.
func runAsTransientTask(scriptPath string) error {
	name := transientTaskName()
	create := exec.Command("schtasks", "/create",
		"/tn", name,
		"/tr", fmt.Sprintf(`cmd /c "%s"`, scriptPath),
		"/sc", "once", "/st", "23:59",
		"/ru", "SYSTEM", "/rl", "HIGHEST", "/f")
	create.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := create.CombinedOutput(); err != nil {
		return fmt.Errorf("create %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}

	run := exec.Command("schtasks", "/run", "/tn", name)
	run.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := run.CombinedOutput(); err != nil {
		// Leave nothing behind if it will not start; the caller falls back.
		del := exec.Command("schtasks", "/delete", "/tn", name, "/f")
		del.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = del.Run()
		return fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
