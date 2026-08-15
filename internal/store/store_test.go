package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func kinds(t *testing.T, st *Store) []string {
	t.Helper()
	events, err := st.RecentEvents(context.Background(), 50)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func TestEnrollAndAuthenticate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id, token, err := st.Enroll(ctx, "vm-01", "linux", "amd64", "0.1.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := st.Authenticate(ctx, id, token); err != nil {
		t.Errorf("authenticate with correct token: %v", err)
	}
	if err := st.Authenticate(ctx, id, "wrong"); err == nil {
		t.Error("authenticate with wrong token: want error, got nil")
	}
	if err := st.Authenticate(ctx, "nosuchdevice", token); err == nil {
		t.Error("authenticate unknown device: want error, got nil")
	}
}

// The enrolled event already announces a new device, so its first check-in must
// not also emit an address-change event.
func TestFirstCheckinDoesNotLogAddressChange(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id, _, err := st.Enroll(ctx, "vm-01", "linux", "amd64", "0.1.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, _, err := st.Checkin(ctx, id, "vm-01", "0.1.0", "10.0.0.5", "", []string{"10.0.0.5"}, nil); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	got := kinds(t, st)
	if len(got) != 1 || got[0] != EventEnrolled {
		t.Errorf("events after first check-in = %v, want only [%s]", got, EventEnrolled)
	}
}

func TestCheckinRecordsOnlyRealChanges(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id, _, err := st.Enroll(ctx, "vm-01", "linux", "amd64", "0.1.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	checkin := func(host, version, remote string, ips []string) {
		t.Helper()
		if _, _, err := st.Checkin(ctx, id, host, version, remote, "", ips, nil); err != nil {
			t.Fatalf("checkin: %v", err)
		}
	}

	checkin("vm-01", "0.1.0", "10.0.0.5", []string{"10.0.0.5"})
	// Three identical heartbeats must add nothing to the timeline.
	checkin("vm-01", "0.1.0", "10.0.0.5", []string{"10.0.0.5"})
	checkin("vm-01", "0.1.0", "10.0.0.5", []string{"10.0.0.5"})
	if got := kinds(t, st); len(got) != 1 {
		t.Errorf("repeat heartbeats produced %v, want no new events", got)
	}

	checkin("vm-01", "0.1.0", "10.0.0.9", []string{"10.0.0.9"})
	checkin("renamed", "0.2.0", "10.0.0.9", []string{"10.0.0.9"})

	got := kinds(t, st)
	want := map[string]bool{EventIPChanged: false, EventRenamed: false, EventUpgraded: false}
	for _, k := range got {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for kind, seen := range want {
		if !seen {
			t.Errorf("missing %s event; got %v", kind, got)
		}
	}
}

func TestSweepOfflineAndRecovery(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id, _, err := st.Enroll(ctx, "vm-01", "linux", "amd64", "0.1.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, _, err := st.Checkin(ctx, id, "vm-01", "0.1.0", "10.0.0.5", "", []string{"10.0.0.5"}, nil); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	// A device that just checked in is never swept, whatever the grace factor.
	if n, err := st.SweepOffline(ctx, 0); err != nil || n != 0 {
		t.Fatalf("sweep of a fresh device = (%d, %v), want (0, nil)", n, err)
	}

	// Timestamps have one-second resolution, so let the clock advance past it
	// rather than reaching into the database to fake a stale heartbeat.
	time.Sleep(1100 * time.Millisecond)
	n, err := st.SweepOffline(ctx, 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d devices, want 1", n)
	}

	devices, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if devices[0].Status != "offline" {
		t.Errorf("status = %q, want offline", devices[0].Status)
	}

	// A second sweep must not re-log an already-offline device.
	if n, err := st.SweepOffline(ctx, 0); err != nil || n != 0 {
		t.Errorf("repeat sweep = (%d, %v), want (0, nil)", n, err)
	}

	// Recovery: the next heartbeat flips it back and logs the transition.
	if _, _, err := st.Checkin(ctx, id, "vm-01", "0.1.0", "10.0.0.5", "", []string{"10.0.0.5"}, nil); err != nil {
		t.Fatalf("recovery checkin: %v", err)
	}
	got := kinds(t, st)
	if len(got) == 0 || got[0] != EventOnline {
		t.Errorf("newest event = %v, want %s first", got, EventOnline)
	}
}

func TestIntervalSettings(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if got, err := st.DefaultCheckinInterval(ctx); err != nil || got != DefaultInterval {
		t.Fatalf("default interval = (%d, %v), want (%d, nil)", got, err, DefaultInterval)
	}
	if err := st.SetDefaultCheckinInterval(ctx, 15); err != nil {
		t.Fatalf("set interval: %v", err)
	}
	if got, _ := st.DefaultCheckinInterval(ctx); got != 15 {
		t.Errorf("interval = %d, want 15", got)
	}

	for _, bad := range []int{0, 4, -1, 90000} {
		if err := st.SetDefaultCheckinInterval(ctx, bad); err == nil {
			t.Errorf("SetDefaultCheckinInterval(%d) = nil, want error", bad)
		}
	}

	// A device enrolled before the change still reports the current default.
	id, _, err := st.Enroll(ctx, "vm-01", "linux", "amd64", "0.1.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	interval, _, err := st.Checkin(ctx, id, "vm-01", "0.1.0", "10.0.0.5", "", nil, nil)
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if interval != 15 {
		t.Errorf("check-in returned interval %d, want 15", interval)
	}
}

func TestEnrollTokenRotation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	first, err := st.EnrollToken(ctx)
	if err != nil || first == "" {
		t.Fatalf("enroll token = (%q, %v), want a generated token", first, err)
	}

	// An enrolled device must survive rotation: it authenticates with its own
	// token, not the shared enrollment secret.
	id, token, err := st.Enroll(ctx, "vm-01", "linux", "amd64", "0.1.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	second, err := st.RotateEnrollToken(ctx)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if second == first {
		t.Error("rotated token is unchanged")
	}
	if err := st.Authenticate(ctx, id, token); err != nil {
		t.Errorf("device auth after rotation: %v", err)
	}
}

func TestDeleteDevice(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id, token, err := st.Enroll(ctx, "vm-01", "linux", "amd64", "0.1.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := st.DeleteDevice(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteDevice(ctx, id); err != ErrNotFound {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
	// The agent must now fail auth, which is what triggers its re-enrollment.
	if err := st.Authenticate(ctx, id, token); err != ErrNotFound {
		t.Errorf("auth after delete = %v, want ErrNotFound", err)
	}
	// Events cascade with the device.
	if got := kinds(t, st); len(got) != 0 {
		t.Errorf("events after delete = %v, want none", got)
	}
}
