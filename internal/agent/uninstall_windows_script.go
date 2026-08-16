// The teardown script is built here rather than in uninstall_windows.go, and
// without a build tag, so it can be tested from any machine. It is Windows-only
// logic — the batch file it produces means nothing elsewhere — but it is also
// pure string assembly, and the last two retirement bugs were both mistakes in
// exactly that: a "%%i" that is only correct inside a file, and a sleep that
// silently did not sleep. Neither needed Windows to catch, only a test.

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// taskName is the scheduled task the installer registers for the agent.
const taskName = "monitorrr-agent"

// uninstallScript builds the teardown batch file, and the human-readable steps
// that go with it.
//
// A batch file rather than a command line passed to "cmd /c": the retry loop
// needs "%%i", which is only correct inside a file — on a command line it has
// to be "%i", and the earlier version got this wrong, so the loop failed to
// parse and no binary was ever deleted. A file also has no length limit, which
// a scheduled task's /tr argument very much does.
func uninstallScript(plan uninstallPlan) (script string, steps []string) {
	logPath := plan.UninstallLog()
	note := func(message, path string) string {
		if logPath == "" {
			return "rem " + message + " " + path
		}
		return fmt.Sprintf(`echo %%DATE%% %%TIME%% %s %s>>"%s"`, message, path, logPath)
	}

	lines := []string{
		"@echo off",
		note("monitorrr uninstall starting for", plan.ExePath),
		// ping, not timeout: timeout refuses to run without a console ("input
		// redirection is not supported") and exits immediately, so every wait
		// in the previous version was silently no time at all.
		"ping -n 4 127.0.0.1 >nul 2>&1",
	}

	if plan.StatePath != "" {
		lines = append(lines, fmt.Sprintf(`del /f /q "%s" >nul 2>&1`, plan.StatePath))
		steps = append(steps, "remove "+plan.StatePath)
	}

	// Windows will not delete a running image, and how long our own exit takes
	// is not ours to know — so each candidate is retried until it goes.
	for _, binary := range plan.BinaryPaths() {
		lines = append(lines,
			fmt.Sprintf(`for /l %%%%i in (1,1,10) do @if exist "%s" (del /f /q "%s" >nul 2>&1 & ping -n 3 127.0.0.1 >nul 2>&1)`,
				binary, binary),
			fmt.Sprintf(`if exist "%s" (%s) else (%s)`,
				binary, note("FAILED to remove", binary), note("removed", binary)))
		steps = append(steps, "remove "+binary)
	}

	if plan.RemoveStateDir {
		// rmdir without /s: only removes the directory when it is empty, which
		// it will not be — the retirement marker and this log both live there.
		lines = append(lines, fmt.Sprintf(`rmdir "%s" >nul 2>&1`, plan.StateDir))
		steps = append(steps, "remove "+plan.StateDir+" (if empty; the retirement marker is kept)")
	}

	// The agent's own task goes last. Deleting a task stops it, and stopping it
	// is what used to kill this helper mid-script.
	lines = append(lines,
		fmt.Sprintf(`schtasks /delete /tn "%s" /f >nul 2>&1`, taskName),
		note("removed the scheduled task", taskName))
	steps = append(steps, "delete the scheduled task "+taskName)

	// Clean up after ourselves: the transient task first, then this file. Both
	// are best-effort — everything that matters has already happened.
	lines = append(lines,
		fmt.Sprintf(`schtasks /delete /tn "%s" /f >nul 2>&1`, transientTaskName()),
		`del /f /q "%~f0" >nul 2>&1`)

	return strings.Join(lines, "\r\n") + "\r\n", steps
}

// writeUninstallScript puts the batch file beside the retirement marker, or in
// the temp directory when there is no state directory to use.
func writeUninstallScript(plan uninstallPlan, script string) (string, error) {
	dir := plan.StateDir
	if dir == "" {
		dir = os.TempDir()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	path := filepath.Join(dir, "uninstall.cmd")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// transientTaskName carries the process id so a leftover task from an earlier
// attempt cannot collide with this one.
func transientTaskName() string {
	return fmt.Sprintf("monitorrr-uninstall-%d", os.Getpid())
}
