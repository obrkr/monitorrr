package agent

import (
	"strings"
	"testing"
)

// windowsPlan pins the install path too: BinaryPaths falls back to it, and its
// default is per-OS, so without this the script under test differs depending on
// which machine runs the suite.
func windowsPlan(t *testing.T) uninstallPlan {
	t.Helper()
	original := installPath
	installPath = `C:\Program Files\monitorrr\monitorrr-agent.exe`
	t.Cleanup(func() { installPath = original })

	return uninstallPlan{
		ExePath:        `C:\Program Files\monitorrr\monitorrr-agent.exe`,
		StatePath:      `C:\ProgramData\monitorrr\agent.json`,
		StateDir:       `C:\ProgramData\monitorrr`,
		RemoveBinary:   true,
		RemoveStateDir: true,
	}
}

// The retry loop is written as a batch file and must use "%%i". An earlier
// version passed the same text to "cmd /c", where the correct form is "%i", so
// the loop failed to parse and no binary was ever removed — silently, because
// everything was redirected to nul.
func TestUninstallScriptUsesBatchPercentEscaping(t *testing.T) {
	script, _ := uninstallScript(windowsPlan(t))

	if !strings.Contains(script, "for /l %%i in (1,1,10)") {
		t.Errorf("retry loop is not written for a batch file:\n%s", script)
	}
	if strings.Contains(script, "%%%%i") {
		t.Error("percent escaping was doubled again")
	}
}

// timeout refuses to run without a console and exits immediately, so every wait
// in the previous version was no wait at all — which matters because Windows
// cannot delete a running executable and the whole point is to wait for it.
func TestUninstallScriptDoesNotUseTimeout(t *testing.T) {
	script, _ := uninstallScript(windowsPlan(t))

	if strings.Contains(script, "timeout ") {
		t.Errorf("teardown uses timeout, which does not wait without a console:\n%s", script)
	}
	if !strings.Contains(script, "ping -n") {
		t.Error("teardown has no working sleep")
	}
}

// Deleting the agent's task stops it, and stopping it is what killed the helper
// mid-script. Everything else has to be done before that point.
func TestUninstallScriptDeletesTheTaskLast(t *testing.T) {
	plan := windowsPlan(t)
	script, _ := uninstallScript(plan)

	taskDelete := strings.Index(script, `schtasks /delete /tn "`+taskName+`"`)
	if taskDelete < 0 {
		t.Fatalf("the agent's scheduled task is never deleted:\n%s", script)
	}
	for _, earlier := range []string{plan.StatePath, plan.ExePath} {
		if at := strings.Index(script, "del /f /q \""+earlier+"\""); at < 0 || at > taskDelete {
			t.Errorf("%s is removed after the task is deleted, or not at all", earlier)
		}
	}
}

// The outcome of removing the binary is the one thing that must not be lost:
// "rm -f" style silence is what let a retired agent keep its executable through
// three rounds of testing.
func TestUninstallScriptRecordsWhetherTheBinaryWent(t *testing.T) {
	plan := windowsPlan(t)
	script, _ := uninstallScript(plan)

	for _, want := range []string{"FAILED to remove", "removed", plan.UninstallLog()} {
		if !strings.Contains(script, want) {
			t.Errorf("teardown does not record %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, "monitorrr uninstall starting") {
		t.Error("teardown does not record that it started, so a helper that dies early leaves no trace")
	}
}

// An agent that was never installed as a task is someone's manual run, and its
// binary is not ours to delete.
func TestUninstallScriptLeavesAnUnmanagedBinaryAlone(t *testing.T) {
	plan := windowsPlan(t)
	plan.RemoveBinary = false

	script, steps := uninstallScript(plan)
	if strings.Contains(script, plan.ExePath) && strings.Contains(script, "del /f /q \""+plan.ExePath+"\"") {
		t.Errorf("an unmanaged binary is deleted anyway:\n%s", script)
	}
	for _, s := range steps {
		if strings.Contains(s, "remove "+plan.ExePath) {
			t.Error("the plan claims it will remove a binary it must not touch")
		}
	}
}

// Batch files want CRLF, and cmd is unforgiving about a file it cannot parse.
func TestUninstallScriptIsCRLFTerminated(t *testing.T) {
	script, _ := uninstallScript(windowsPlan(t))

	if strings.Contains(strings.ReplaceAll(script, "\r\n", ""), "\n") {
		t.Error("the batch file contains bare newlines")
	}
	if !strings.HasSuffix(script, "\r\n") {
		t.Error("the batch file does not end with a newline")
	}
}

// Paths go into the batch file verbatim, inside quotes. An earlier version
// formatted them with %q, which is Go's quoting, not cmd's: every backslash
// came out doubled, so every path in the script pointed somewhere that does not
// exist and the teardown removed nothing at all. It looked correct in review.
func TestUninstallScriptDoesNotEscapeBackslashes(t *testing.T) {
	plan := windowsPlan(t)
	script, _ := uninstallScript(plan)

	if strings.Contains(script, `\\`) {
		t.Errorf("paths are Go-quoted rather than cmd-quoted:\n%s", script)
	}
	for _, want := range []string{plan.ExePath, plan.StatePath, plan.StateDir, plan.UninstallLog()} {
		if !strings.Contains(script, want) {
			t.Errorf("%q does not appear in the script as written:\n%s", want, script)
		}
	}
}
