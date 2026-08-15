package store

import (
	"context"
	"testing"
	"time"
)

func TestCollectionLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	device := mustDevice(t, st, "vm-01", "linux")

	c, err := st.RequestCollection(ctx, device, "/var/log/syslog", "tester")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if c.State != CollectQueued {
		t.Errorf("state = %q, want %q", c.State, CollectQueued)
	}

	// Claiming hands it over exactly once.
	claimed, err := st.ClaimCollections(ctx, device)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Path != "/var/log/syslog" {
		t.Fatalf("claimed = %+v, want the queued request", claimed)
	}
	again, err := st.ClaimCollections(ctx, device)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second claim returned %d, want 0", len(again))
	}

	// Size is known before any bytes move.
	if err := st.RecordCollectionSize(ctx, c.ID, device, "syslog", 4096, false, ""); err != nil {
		t.Fatalf("record size: %v", err)
	}
	got, _ := st.GetCollection(ctx, c.ID)
	if got.State != CollectTransferring || got.Size != 4096 {
		t.Errorf("after probe: state=%q size=%d, want transferring/4096", got.State, got.Size)
	}
	if got.Progress != 0 {
		t.Errorf("progress = %d before any bytes, want 0", got.Progress)
	}

	if err := st.UpdateCollectionProgress(ctx, c.ID, 1024); err != nil {
		t.Fatalf("progress: %v", err)
	}
	got, _ = st.GetCollection(ctx, c.ID)
	if got.Progress != 25 {
		t.Errorf("progress = %d at a quarter transferred, want 25", got.Progress)
	}

	if err := st.CompleteCollection(ctx, c.ID, device, "abc123", 4096); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _ = st.GetCollection(ctx, c.ID)
	if got.State != CollectDone || got.Progress != 100 {
		t.Errorf("after completion: state=%q progress=%d", got.State, got.Progress)
	}
	if got.ExpiresAt == nil {
		t.Fatal("no expiry set — the file would be kept forever")
	}
	// The retention clock starts at completion, not at request.
	if d := time.Until(*got.ExpiresAt); d < CollectRetention-time.Minute || d > CollectRetention+time.Minute {
		t.Errorf("expires in %s, want about %s", d, CollectRetention)
	}

	// The pull is on the device's timeline.
	found := false
	for _, k := range kinds(t, st) {
		if k == EventCollect {
			found = true
		}
	}
	if !found {
		t.Error("completed collection did not write a device event")
	}
}

// A probe failure ends the collection before any transfer is attempted.
func TestCollectionProbeFailure(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	device := mustDevice(t, st, "vm-01", "linux")

	c, err := st.RequestCollection(ctx, device, "/nope", "tester")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := st.ClaimCollections(ctx, device); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.RecordCollectionSize(ctx, c.ID, device, "", 0, false, "no such file: /nope"); err != nil {
		t.Fatalf("record probe failure: %v", err)
	}

	got, _ := st.GetCollection(ctx, c.ID)
	if got.State != CollectFailed {
		t.Errorf("state = %q, want %q", got.State, CollectFailed)
	}
	if got.Error == "" {
		t.Error("a failed collection must say why")
	}
}

// Retention is the whole point of the feature, and seven days is not something
// a test can wait out — so the clock is moved instead.
func TestCollectionRetentionExpiry(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	device := mustDevice(t, st, "vm-01", "linux")

	fresh, _ := st.RequestCollection(ctx, device, "/var/log/fresh", "tester")
	old, _ := st.RequestCollection(ctx, device, "/var/log/old", "tester")
	for _, c := range []Collection{fresh, old} {
		if err := st.RecordCollectionSize(ctx, c.ID, device, "log", 10, false, ""); err != nil {
			t.Fatalf("record size: %v", err)
		}
		if err := st.CompleteCollection(ctx, c.ID, device, "digest", 10); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}

	// Nothing is due yet.
	expired, err := st.ExpiredCollections(ctx)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("%d collections due for deletion immediately, want 0", len(expired))
	}

	// Backdate one past its retention.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE collections SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Hour).Unix(), old.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	expired, err = st.ExpiredCollections(ctx)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != old.ID {
		t.Fatalf("expired = %+v, want only the backdated one", expired)
	}

	if err := st.MarkCollectionExpired(ctx, old.ID); err != nil {
		t.Fatalf("mark expired: %v", err)
	}
	got, _ := st.GetCollection(ctx, old.ID)
	if got.State != CollectExpired {
		t.Errorf("state = %q, want %q", got.State, CollectExpired)
	}
	// The record survives deletion of the bytes: knowing a file was pulled, by
	// whom, and that it is gone is the audit trail.
	if got.Path != "/var/log/old" || got.CreatedBy != "tester" {
		t.Error("expiring a collection destroyed its audit record")
	}
	if got.SHA256 != "" {
		t.Error("digest kept for a file that no longer exists")
	}

	// It is not offered up a second time.
	expired, _ = st.ExpiredCollections(ctx)
	if len(expired) != 0 {
		t.Errorf("already-expired collection still listed as due: %+v", expired)
	}

	// And the fresh one is untouched.
	stillThere, _ := st.GetCollection(ctx, fresh.ID)
	if stillThere.State != CollectDone {
		t.Errorf("fresh collection state = %q, want %q", stillThere.State, CollectDone)
	}
}

// A transfer whose agent vanished must not sit at "transferring" forever.
func TestSweepStalledCollections(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	device := mustDevice(t, st, "vm-01", "linux")

	c, _ := st.RequestCollection(ctx, device, "/var/log/stalled", "tester")
	if _, err := st.ClaimCollections(ctx, device); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.RecordCollectionSize(ctx, c.ID, device, "stalled", 1000, false, ""); err != nil {
		t.Fatalf("record size: %v", err)
	}

	// Recent transfers are left alone.
	if n, err := st.SweepStalledCollections(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("early sweep = (%d, %v), want (0, nil)", n, err)
	}

	if _, err := st.db.ExecContext(ctx,
		`UPDATE collections SET created_at = ? WHERE id = ?`,
		time.Now().Add(-24*time.Hour).Unix(), c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := st.SweepStalledCollections(ctx, time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	got, _ := st.GetCollection(ctx, c.ID)
	if got.State != CollectFailed || got.Error == "" {
		t.Errorf("stalled transfer = %q / %q, want failed with a reason", got.State, got.Error)
	}
}

func TestCollectionValidation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	device := mustDevice(t, st, "vm-01", "linux")

	if _, err := st.RequestCollection(ctx, device, "   ", "tester"); err == nil {
		t.Error("blank path: want an error")
	}
	if _, err := st.RequestCollection(ctx, "nosuchdevice", "/etc/hosts", "tester"); err != ErrNotFound {
		t.Errorf("unknown device = %v, want ErrNotFound", err)
	}

	// A machine on its way out should not be asked for files.
	if err := st.RetireDevice(ctx, device); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := st.RequestCollection(ctx, device, "/etc/hosts", "tester"); err == nil {
		t.Error("collection from a retiring device: want an error")
	}
}
