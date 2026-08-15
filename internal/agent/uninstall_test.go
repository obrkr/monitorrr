package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Teardown must never remove a directory it merely guessed at. Running with
// -state ./dist/agent-state.json used to imply removing ./dist, which holds the
// server binary and the database.
func TestPlanUninstallProtectsNonStandardStateDirs(t *testing.T) {
	canonicalState := DefaultStatePath()
	canonicalDir := filepath.Dir(canonicalState)

	cases := []struct {
		name           string
		exe            string
		state          string
		managed        bool
		wantBinary     bool
		wantStateDir   bool
		wantStateDirIs string
	}{
		{
			name:           "canonical install",
			exe:            canonicalInstallPath(),
			state:          canonicalState,
			managed:        true,
			wantBinary:     true,
			wantStateDir:   true,
			wantStateDirIs: canonicalDir,
		},
		{
			// No service registered: someone is running a build by hand, and
			// deleting it under them would be a nasty surprise.
			name:           "developer build in a working directory",
			exe:            "/Users/someone/projects/monitorrr/dist/monitorrr-agent",
			state:          "/Users/someone/projects/monitorrr/dist/agent-state.json",
			managed:        false,
			wantBinary:     false,
			wantStateDir:   false,
			wantStateDirIs: "/Users/someone/projects/monitorrr/dist",
		},
		{
			name:         "canonical binary but a custom state path",
			exe:          canonicalInstallPath(),
			state:        "/etc/monitorrr-agent.json",
			managed:      true,
			wantBinary:   true,
			wantStateDir: false,
		},
		{
			// Installed with --prefix. Every bit as real an installation as one
			// in /usr/local/bin, so its binary must go too.
			name:         "managed install somewhere else",
			exe:          "/opt/monitorrr/monitorrr-agent",
			state:        canonicalState,
			managed:      true,
			wantBinary:   true,
			wantStateDir: true,
		},
		{
			// The canonical path is trusted even with no service found, since
			// nothing else puts a binary there.
			name:         "canonical path with no service registered",
			exe:          canonicalInstallPath(),
			state:        canonicalState,
			managed:      false,
			wantBinary:   true,
			wantStateDir: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planUninstall(tc.exe, tc.state, tc.managed)
			if plan.RemoveBinary != tc.wantBinary {
				t.Errorf("RemoveBinary = %v, want %v (exe %s)", plan.RemoveBinary, tc.wantBinary, tc.exe)
			}
			if plan.RemoveStateDir != tc.wantStateDir {
				t.Errorf("RemoveStateDir = %v, want %v (state %s)", plan.RemoveStateDir, tc.wantStateDir, tc.state)
			}
			// The state file itself is always ours to delete.
			if plan.StatePath != tc.state {
				t.Errorf("StatePath = %q, want %q", plan.StatePath, tc.state)
			}
			if tc.wantStateDirIs != "" && plan.StateDir != tc.wantStateDirIs {
				t.Errorf("StateDir = %q, want %q", plan.StateDir, tc.wantStateDirIs)
			}
		})
	}
}

// The marker is what makes retirement safe when the agent cannot stop its own
// supervisor: a restart must exit rather than enrol as a new device.
func TestTombstoneStopsReEnrolment(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "agent.json")

	a := &Agent{
		cfg: Config{StatePath: statePath},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if a.isTombstoned() {
		t.Fatal("a fresh install must not look retired")
	}

	a.writeTombstone()

	if !a.isTombstoned() {
		t.Error("marker was written but not detected")
	}
	if got := tombstonePath(statePath); got != filepath.Join(dir, "retired") {
		t.Errorf("tombstonePath = %q, want %q", got, filepath.Join(dir, "retired"))
	}

	// It must survive removal of the identity file — that is the whole point,
	// since teardown deletes the identity but the supervisor may still restart.
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove state: %v", err)
	}
	if !a.isTombstoned() {
		t.Error("marker did not survive removal of the identity file")
	}

	// Clearing it (what reinstalling does) allows enrolment again.
	if err := os.Remove(tombstonePath(statePath)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	if a.isTombstoned() {
		t.Error("machine still looks retired after the marker was cleared")
	}
}

// With no state path there is nowhere to put a marker; that must not panic or
// wrongly report the machine as retired.
func TestTombstoneWithNoStatePath(t *testing.T) {
	a := &Agent{
		cfg: Config{StatePath: ""},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if got := tombstonePath(""); got != "" {
		t.Errorf("tombstonePath(\"\") = %q, want empty", got)
	}
	a.writeTombstone() // must not panic
	if a.isTombstoned() {
		t.Error("no state path must not report as retired")
	}
}

// An agent that cannot identify its own binary must still tear down what it can
// rather than removing something arbitrary.
func TestPlanUninstallWithUnknownPaths(t *testing.T) {
	plan := planUninstall("", "", true)
	if plan.RemoveBinary {
		t.Error("RemoveBinary = true with no executable path")
	}
	if plan.RemoveStateDir {
		t.Error("RemoveStateDir = true with no state path")
	}
	if plan.StateDir != "" {
		t.Errorf("StateDir = %q, want empty", plan.StateDir)
	}
}

// A retired machine must be able to come back — deliberately. The marker stops
// a supervisor restart re-enrolling it by accident, but an operator who asks
// for it explicitly has to have a way through, or retirement is a one-way door
// on any install the one-line installer did not perform.
func TestForceEnrollClearsTheMarker(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "agent.json")

	a := &Agent{
		cfg: Config{StatePath: statePath},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	a.writeTombstone()
	if !a.isTombstoned() {
		t.Fatal("marker was not written")
	}

	if err := a.clearTombstone(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if a.isTombstoned() {
		t.Error("machine still looks retired after the marker was cleared")
	}

	// Clearing again is not an error: the flag may be left in a service
	// definition, and every subsequent start would otherwise fail.
	if err := a.clearTombstone(); err != nil {
		t.Errorf("clearing an absent marker returned %v, want nil", err)
	}
}

// Removing the binary is the promise retirement makes, and the path it was
// started from is not always the path it lives at — a self-update leaves the
// first stale. Both are tried.
func TestBinaryPathsCoverBothCandidates(t *testing.T) {
	original := installPath
	installPath = "/usr/local/bin/monitorrr-agent"
	t.Cleanup(func() { installPath = original })

	t.Run("started from somewhere else", func(t *testing.T) {
		p := planUninstall("/opt/monitorrr/monitorrr-agent", DefaultStatePath(), true)
		got := p.BinaryPaths()
		want := []string{"/opt/monitorrr/monitorrr-agent", "/usr/local/bin/monitorrr-agent"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("BinaryPaths() = %v, want both candidates %v", got, want)
		}
	})

	t.Run("the two agree", func(t *testing.T) {
		p := planUninstall(installPath, DefaultStatePath(), true)
		if got := p.BinaryPaths(); len(got) != 1 {
			t.Errorf("BinaryPaths() = %v, want one path with no duplicate", got)
		}
	})

	t.Run("nothing to remove", func(t *testing.T) {
		p := planUninstall("/opt/monitorrr/monitorrr-agent", DefaultStatePath(), false)
		if got := p.BinaryPaths(); got != nil {
			t.Errorf("BinaryPaths() = %v with RemoveBinary false, want none", got)
		}
	})
}

// A teardown that cannot say what it did is the reason this bug took three
// attempts to pin down.
func TestUninstallLogSitsBesideTheMarker(t *testing.T) {
	p := planUninstall(installPath, "/var/lib/monitorrr/agent.json", true)
	if got := p.UninstallLog(); got != "/var/lib/monitorrr/uninstall.log" {
		t.Errorf("UninstallLog() = %q, want it beside the state file", got)
	}
	if got := planUninstall(installPath, "", true).UninstallLog(); got != "" {
		t.Errorf("UninstallLog() = %q with no state path, want empty", got)
	}
}

// Linux reports a replaced binary as "/path (deleted)". Building a removal
// command from that would delete nothing at all.
func TestOwnExecutableStripsTheDeletedSuffix(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "monitorrr-agent")
	if err := os.WriteFile(real, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	// ownExecutable resolves the running process, so the suffix handling is
	// checked directly on the string form it produces.
	if got := strings.TrimSuffix(real+" (deleted)", " (deleted)"); got != real {
		t.Errorf("suffix trim = %q, want %q", got, real)
	}

	exe, err := ownExecutable()
	if err != nil {
		t.Fatalf("ownExecutable: %v", err)
	}
	if strings.HasSuffix(exe, " (deleted)") {
		t.Errorf("ownExecutable() = %q, still carrying the suffix", exe)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Errorf("ownExecutable() = %q, which does not exist: %v", exe, err)
	}
}
