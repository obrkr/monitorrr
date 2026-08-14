package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func mustScript(t *testing.T, st *Store, name, interpreter, content string) Script {
	t.Helper()
	sc, err := st.SaveScript(context.Background(), "", name, "", interpreter, content, 60)
	if err != nil {
		t.Fatalf("save script: %v", err)
	}
	return sc
}

func mustDevice(t *testing.T, st *Store, hostname, goos string) string {
	t.Helper()
	id, _, err := st.Enroll(context.Background(), hostname, goos, "arm64", "0.1.0", "10.0.0.5")
	if err != nil {
		t.Fatalf("enroll %s: %v", hostname, err)
	}
	return id
}

func TestSaveScriptValidation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	cases := []struct {
		name        string
		script      string
		interpreter string
		content     string
		timeout     int
	}{
		{"empty name", "", InterpreterSh, "echo hi", 60},
		{"whitespace name", "   ", InterpreterSh, "echo hi", 60},
		{"unknown interpreter", "x", "python", "print(1)", 60},
		{"empty content", "x", InterpreterSh, "   ", 60},
		{"timeout too large", "x", InterpreterSh, "echo hi", 7200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.SaveScript(ctx, "", tc.script, "", tc.interpreter, tc.content, tc.timeout); err == nil {
				t.Error("want error, got nil")
			}
		})
	}

	// A zero timeout is filled in rather than rejected.
	sc, err := st.SaveScript(ctx, "", "ok", "", InterpreterSh, "echo hi", 0)
	if err != nil {
		t.Fatalf("save with zero timeout: %v", err)
	}
	if sc.TimeoutSecs != DefaultJobTimeout {
		t.Errorf("timeout = %d, want %d", sc.TimeoutSecs, DefaultJobTimeout)
	}
	if sc.SHA256 == "" {
		t.Error("sha256 was not computed")
	}
}

func TestUpdateScriptRehashes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sc := mustScript(t, st, "probe", InterpreterSh, "echo one")
	updated, err := st.SaveScript(ctx, sc.ID, "probe", "", InterpreterSh, "echo two", 60)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.SHA256 == sc.SHA256 {
		t.Error("hash unchanged after editing content")
	}
	if updated.ID != sc.ID {
		t.Errorf("id changed on update: %s → %s", sc.ID, updated.ID)
	}

	if _, err := st.SaveScript(ctx, "nosuchid", "x", "", InterpreterSh, "echo", 60); err != ErrNotFound {
		t.Errorf("update of unknown script = %v, want ErrNotFound", err)
	}
}

// A PowerShell script must never be queued against a Linux box, and the whole
// dispatch fails rather than partially succeeding.
func TestDispatchRejectsIncompatibleOS(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	linux := mustDevice(t, st, "ubuntu-01", "linux")
	windows := mustDevice(t, st, "win-01", "windows")
	shScript := mustScript(t, st, "df", InterpreterSh, "df -h")
	psScript := mustScript(t, st, "disk", InterpreterPowerShell, "Get-Disk")

	if _, err := st.Dispatch(ctx, psScript.ID, []string{linux}, "tester"); err == nil {
		t.Error("dispatching powershell to linux: want error, got nil")
	}
	if _, err := st.Dispatch(ctx, shScript.ID, []string{windows}, "tester"); err == nil {
		t.Error("dispatching sh to windows: want error, got nil")
	}

	// A mixed selection must not half-commit.
	if _, err := st.Dispatch(ctx, shScript.ID, []string{linux, windows}, "tester"); err == nil {
		t.Error("mixed dispatch: want error, got nil")
	}
	jobs, err := st.ListJobs(ctx, 100)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("failed dispatch left %d jobs behind, want 0", len(jobs))
	}

	if _, err := st.Dispatch(ctx, shScript.ID, nil, "tester"); err == nil {
		t.Error("dispatch with no devices: want error, got nil")
	}
}

func TestClaimJobsDeliversOnce(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	device := mustDevice(t, st, "ubuntu-01", "linux")
	other := mustDevice(t, st, "ubuntu-02", "linux")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")

	if _, err := st.Dispatch(ctx, script.ID, []string{device}, "tester"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	claimed, err := st.ClaimJobs(ctx, device)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(claimed))
	}
	if claimed[0].Content != "df -h" || claimed[0].ScriptSHA256 != script.SHA256 {
		t.Error("claimed job does not carry the script body and hash")
	}

	// Claiming again must return nothing: the job is already in flight.
	again, err := st.ClaimJobs(ctx, device)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second claim returned %d jobs, want 0", len(again))
	}

	// And another device must never see it.
	strangers, err := st.ClaimJobs(ctx, other)
	if err != nil {
		t.Fatalf("claim for other device: %v", err)
	}
	if len(strangers) != 0 {
		t.Errorf("other device claimed %d jobs, want 0", len(strangers))
	}
}

func TestCompleteJob(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	device := mustDevice(t, st, "ubuntu-01", "linux")
	imposter := mustDevice(t, st, "ubuntu-02", "linux")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")

	jobs, err := st.Dispatch(ctx, script.ID, []string{device}, "tester")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	jobID := jobs[0].ID
	if _, err := st.ClaimJobs(ctx, device); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A device must not be able to write results for someone else's job.
	if err := st.CompleteJob(ctx, jobID, imposter, 0, "", "", "", 5, false); err != ErrNotFound {
		t.Errorf("cross-device completion = %v, want ErrNotFound", err)
	}

	if err := st.CompleteJob(ctx, jobID, device, 3, "out", "err", "", 1200, false); err != nil {
		t.Fatalf("complete: %v", err)
	}

	job, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != JobDone {
		t.Errorf("state = %q, want %q", job.State, JobDone)
	}
	if job.ExitCode == nil || *job.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", job.ExitCode)
	}
	if job.Stdout != "out" || job.Stderr != "err" || job.DurationMS != 1200 {
		t.Errorf("output not recorded: %+v", job)
	}
	if job.Content != "df -h" {
		t.Errorf("job did not snapshot the script body, got %q", job.Content)
	}

	// The run shows up on the device timeline.
	found := false
	for _, k := range kinds(t, st) {
		if k == EventJob {
			found = true
		}
	}
	if !found {
		t.Error("completed job did not write a device event")
	}
}

func TestCompleteJobClampsHugeOutput(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	device := mustDevice(t, st, "ubuntu-01", "linux")
	script := mustScript(t, st, "noisy", InterpreterSh, "yes")
	jobs, _ := st.Dispatch(ctx, script.ID, []string{device}, "tester")

	huge := strings.Repeat("x", maxOutput*2)
	if err := st.CompleteJob(ctx, jobs[0].ID, device, 0, huge, "", "", 10, false); err != nil {
		t.Fatalf("complete: %v", err)
	}

	job, err := st.GetJob(ctx, jobs[0].ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if len(job.Stdout) != maxOutput {
		t.Errorf("stdout length = %d, want %d", len(job.Stdout), maxOutput)
	}
	if !job.Truncated {
		t.Error("truncated flag not set on clamped output")
	}
}

func TestSweepLostJobs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	device := mustDevice(t, st, "ubuntu-01", "linux")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")
	jobs, _ := st.Dispatch(ctx, script.ID, []string{device}, "tester")
	if _, err := st.ClaimJobs(ctx, device); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A job dispatched moments ago is still legitimately running.
	if n, err := st.SweepLostJobs(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep = (%d, %v), want (0, nil)", n, err)
	}

	// Backdate the dispatch rather than waiting out the real grace period.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE jobs SET dispatched_at = ?`, time.Now().Add(-24*time.Hour).Unix()); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := st.SweepLostJobs(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d jobs, want 1", n)
	}

	job, err := st.GetJob(ctx, jobs[0].ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != JobLost {
		t.Errorf("state = %q, want %q", job.State, JobLost)
	}
	if job.Error == "" {
		t.Error("lost job has no explanatory error")
	}

	if pending, err := st.PendingJobCount(ctx); err != nil || pending != 0 {
		t.Errorf("pending after sweep = (%d, %v), want (0, nil)", pending, err)
	}
}

// Deleting a script must not erase the record of what already ran.
func TestDeleteScriptKeepsRunHistory(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	device := mustDevice(t, st, "ubuntu-01", "linux")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")
	jobs, _ := st.Dispatch(ctx, script.ID, []string{device}, "tester")
	if err := st.CompleteJob(ctx, jobs[0].ID, device, 0, "ok", "", "", 10, false); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if err := st.DeleteScript(ctx, script.ID); err != nil {
		t.Fatalf("delete script: %v", err)
	}

	job, err := st.GetJob(ctx, jobs[0].ID)
	if err != nil {
		t.Fatalf("get job after script deletion: %v", err)
	}
	if job.Content != "df -h" || job.ScriptName != "df" {
		t.Errorf("run history lost its script snapshot: %+v", job)
	}
	if job.ScriptID != "" {
		t.Errorf("script_id = %q, want empty after the script was deleted", job.ScriptID)
	}
}

// Removing a device takes its job history with it, matching the "forget this
// machine" semantics of the dashboard button.
func TestDeleteDeviceCascadesToJobs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	device := mustDevice(t, st, "ubuntu-01", "linux")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")
	if _, err := st.Dispatch(ctx, script.ID, []string{device}, "tester"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := st.DeleteDevice(ctx, device); err != nil {
		t.Fatalf("delete device: %v", err)
	}

	jobs, err := st.ListJobs(ctx, 100)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("jobs remaining after device deletion = %d, want 0", len(jobs))
	}
}
