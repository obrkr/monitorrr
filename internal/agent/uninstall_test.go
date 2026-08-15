package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
		wantBinary     bool
		wantStateDir   bool
		wantStateDirIs string
	}{
		{
			name:           "canonical install",
			exe:            canonicalInstallPath(),
			state:          canonicalState,
			wantBinary:     true,
			wantStateDir:   true,
			wantStateDirIs: canonicalDir,
		},
		{
			name:           "developer build in a working directory",
			exe:            "/Users/someone/projects/monitorrr/dist/monitorrr-agent",
			state:          "/Users/someone/projects/monitorrr/dist/agent-state.json",
			wantBinary:     false,
			wantStateDir:   false,
			wantStateDirIs: "/Users/someone/projects/monitorrr/dist",
		},
		{
			name:         "canonical binary but a custom state path",
			exe:          canonicalInstallPath(),
			state:        "/etc/monitorrr-agent.json",
			wantBinary:   true,
			wantStateDir: false,
		},
		{
			name:         "unknown binary location, canonical state",
			exe:          "/opt/monitorrr/monitorrr-agent",
			state:        canonicalState,
			wantBinary:   false,
			wantStateDir: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planUninstall(tc.exe, tc.state)
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
	plan := planUninstall("", "")
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
