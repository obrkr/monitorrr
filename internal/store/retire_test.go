package store

import (
	"context"
	"testing"
	"time"
)

// modernFeatures is what a current agent advertises on check-in. An agent
// predating capability advertisement sends nothing, which is the signal that it
// cannot act on newer instructions.
var modernFeatures = []string{"jobs", "retire"}

func TestRetireLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id := mustDevice(t, st, "vm-01", "linux")
	if _, _, err := st.Checkin(ctx, id, "vm-01", "0.1.0", "10.0.0.5", []string{"10.0.0.5"}, nil); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	// Before retirement, check-ins carry no retire instruction.
	if _, retire, err := st.Checkin(ctx, id, "vm-01", "0.1.0", "10.0.0.5", []string{"10.0.0.5"}, modernFeatures); err != nil || retire {
		t.Fatalf("checkin before retire = (retire=%v, %v), want (false, nil)", retire, err)
	}

	if err := st.RetireDevice(ctx, id); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// Requested but not yet acknowledged: visible, with a timestamp set.
	devices, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if devices[0].RetiredAt == nil {
		t.Error("retired_at not set after retirement was requested")
	}
	if devices[0].Status == StatusRetired {
		t.Error("status is already retired before the agent acknowledged")
	}

	// The next check-in delivers the instruction.
	_, retire, err := st.Checkin(ctx, id, "vm-01", "0.1.0", "10.0.0.5", []string{"10.0.0.5"}, modernFeatures)
	if err != nil {
		t.Fatalf("checkin after retire: %v", err)
	}
	if !retire {
		t.Fatal("check-in did not carry the retire instruction")
	}

	if err := st.CompleteRetirement(ctx, id); err != nil {
		t.Fatalf("complete retirement: %v", err)
	}
	devices, _ = st.ListDevices(ctx)
	if devices[0].Status != StatusRetired {
		t.Errorf("status = %q, want %q", devices[0].Status, StatusRetired)
	}

	// The record and its timeline survive: knowing a machine was decommissioned
	// is the whole reason to retire rather than delete.
	var sawRequest, sawDone bool
	for _, k := range kinds(t, st) {
		switch k {
		case EventRetiring:
			sawRequest = true
		case EventRetired:
			sawDone = true
		}
	}
	if !sawRequest || !sawDone {
		t.Errorf("timeline missing retirement events: %v", kinds(t, st))
	}
}

func TestRetireIsIdempotentAndValidated(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id := mustDevice(t, st, "vm-01", "linux")
	if err := st.RetireDevice(ctx, id); err != nil {
		t.Fatalf("first retire: %v", err)
	}
	// Asking twice is harmless — the operator may click again while waiting for
	// an offline machine to come back.
	if err := st.RetireDevice(ctx, id); err != nil {
		t.Errorf("second retire: %v", err)
	}
	if err := st.RetireDevice(ctx, "nosuchdevice"); err != ErrNotFound {
		t.Errorf("retire unknown device = %v, want ErrNotFound", err)
	}
	if err := st.CompleteRetirement(ctx, "nosuchdevice"); err != ErrNotFound {
		t.Errorf("ack for unknown device = %v, want ErrNotFound", err)
	}
}

// An agent must not be able to claim it retired when nobody asked it to.
func TestCompleteRetirementRequiresRequest(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id := mustDevice(t, st, "vm-01", "linux")
	if err := st.CompleteRetirement(ctx, id); err != ErrNotFound {
		t.Errorf("unrequested retirement ack = %v, want ErrNotFound", err)
	}
}

func TestRetiringDeviceIsNotSweptOffline(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id := mustDevice(t, st, "vm-01", "linux")
	if _, _, err := st.Checkin(ctx, id, "vm-01", "0.1.0", "10.0.0.5", []string{"10.0.0.5"}, nil); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if err := st.RetireDevice(ctx, id); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// A machine being decommissioned going quiet is expected, not an incident.
	time.Sleep(1100 * time.Millisecond)
	if n, err := st.SweepOffline(ctx, 0); err != nil || n != 0 {
		t.Errorf("sweep of a retiring device = (%d, %v), want (0, nil)", n, err)
	}
}

func TestRetireCancelsQueuedJobsAndBlocksNewOnes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id := mustDevice(t, st, "vm-01", "linux")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")
	jobs, err := st.Dispatch(ctx, script.ID, []string{id}, "tester")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if err := st.RetireDevice(ctx, id); err != nil {
		t.Fatalf("retire: %v", err)
	}

	job, err := st.GetJob(ctx, jobs[0].ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State == JobQueued {
		t.Error("queued job survived retirement — it would never run")
	}
	if job.Error == "" {
		t.Error("cancelled job has no explanation")
	}

	// And nothing new can be aimed at it.
	if _, err := st.Dispatch(ctx, script.ID, []string{id}, "tester"); err == nil {
		t.Error("dispatch to a retiring device: want error, got nil")
	}

	// A retiring agent is told to uninstall and given no work.
	claimed, err := st.ClaimJobs(ctx, id)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("retiring device claimed %d jobs, want 0", len(claimed))
	}
}

// An agent too old to understand retirement keeps checking in and ignoring the
// instruction. The check-in counter plus the absent capability is what lets the
// dashboard say so instead of showing "Retiring…" forever.
func TestStuckRetirementIsDetectable(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	old := mustDevice(t, st, "old-agent", "linux")
	current := mustDevice(t, st, "new-agent", "linux")

	// An old agent sends no feature list at all.
	if _, _, err := st.Checkin(ctx, old, "old-agent", "0.1.0", "10.0.0.5", nil, nil); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if _, _, err := st.Checkin(ctx, current, "new-agent", "0.2.0", "10.0.0.6", nil, modernFeatures); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	byName := func() map[string]Device {
		t.Helper()
		devices, err := st.ListDevices(ctx)
		if err != nil {
			t.Fatalf("list devices: %v", err)
		}
		out := map[string]Device{}
		for _, d := range devices {
			out[d.Hostname] = d
		}
		return out
	}

	devices := byName()
	if devices["old-agent"].SupportsRetire() {
		t.Error("an agent advertising nothing must not be reported as retire-capable")
	}
	if !devices["new-agent"].SupportsRetire() {
		t.Error("an agent advertising retire must be reported as capable")
	}

	if err := st.RetireDevice(ctx, old); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got := byName()["old-agent"].RetireCheckins; got != 0 {
		t.Errorf("retire_checkins = %d immediately after the request, want 0", got)
	}

	// Each subsequent heartbeat is evidence the agent is alive and ignoring us.
	for i := 1; i <= 3; i++ {
		if _, _, err := st.Checkin(ctx, old, "old-agent", "0.1.0", "10.0.0.5", nil, nil); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
		if got := byName()["old-agent"].RetireCheckins; got != i {
			t.Errorf("after %d check-ins retire_checkins = %d, want %d", i, got, i)
		}
	}

	// A device that never checks in keeps a count of zero, which is how a
	// powered-off machine stays distinguishable from one that is ignoring us.
	if err := st.RetireDevice(ctx, current); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got := byName()["new-agent"].RetireCheckins; got != 0 {
		t.Errorf("silent device retire_checkins = %d, want 0", got)
	}
}

// A database created before retirement existed must gain the column on open.
func TestMigrationAddsRetiredAtColumn(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/legacy.db"

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Simulate the pre-retirement schema by dropping the column back off.
	if _, err := st.db.Exec(`ALTER TABLE devices DROP COLUMN retired_at`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	st.Close()

	// Reopening must migrate it back rather than failing every query.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	id := mustDevice(t, st2, "vm-01", "linux")
	if _, _, err := st2.Checkin(context.Background(), id, "vm-01", "0.1.0", "10.0.0.5", nil, nil); err != nil {
		t.Errorf("checkin after migration: %v", err)
	}
}
