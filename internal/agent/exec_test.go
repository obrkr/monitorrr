package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"strings"
	"testing"

	"github.com/ollie/monitorrr/internal/proto"
)

// job builds a well-formed job with a correct checksum.
func job(script string, timeout int) proto.Job {
	sum := sha256.Sum256([]byte(script))
	return proto.Job{
		ID:          "test-job",
		Interpreter: "sh",
		Script:      script,
		SHA256:      hex.EncodeToString(sum[:]),
		TimeoutSecs: timeout,
	}
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script execution is not available on windows")
	}
}

func TestRunJobCapturesOutput(t *testing.T) {
	skipOnWindows(t)

	res := runJob(context.Background(), job("echo out; echo err >&2", 30))
	if res.Error != "" {
		t.Fatalf("unexpected execution error: %s", res.Error)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "out" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "out")
	}
	if strings.TrimSpace(res.Stderr) != "err" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "err")
	}
}

// A script that runs and exits non-zero is a normal result, not a failure to
// execute — the distinction matters on the Runs page.
func TestRunJobNonZeroExitIsNotAnError(t *testing.T) {
	skipOnWindows(t)

	res := runJob(context.Background(), job("exit 42", 30))
	if res.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", res.ExitCode)
	}
	if res.Error != "" {
		t.Errorf("Error = %q, want empty for a clean non-zero exit", res.Error)
	}
}

func TestRunJobRejectsChecksumMismatch(t *testing.T) {
	skipOnWindows(t)

	tampered := job("echo hello", 30)
	tampered.Script = "echo tampered" // hash now describes different content

	res := runJob(context.Background(), tampered)
	if !strings.Contains(res.Error, "checksum mismatch") {
		t.Errorf("Error = %q, want a checksum mismatch", res.Error)
	}
	if res.Stdout != "" {
		t.Errorf("tampered script produced output %q — it must not run at all", res.Stdout)
	}
}

func TestRunJobTimesOut(t *testing.T) {
	skipOnWindows(t)

	res := runJob(context.Background(), job("sleep 30", 1))
	if !strings.Contains(res.Error, "timed out") {
		t.Errorf("Error = %q, want a timeout", res.Error)
	}
	if res.DurationMS > 10_000 {
		t.Errorf("took %dms — the timeout did not stop the script", res.DurationMS)
	}
}

func TestRunJobRejectsUnknownInterpreter(t *testing.T) {
	j := job("echo hi", 30)
	j.Interpreter = "python"

	res := runJob(context.Background(), j)
	if !strings.Contains(res.Error, "unsupported interpreter") {
		t.Errorf("Error = %q, want an unsupported-interpreter error", res.Error)
	}
}

func TestRunJobRejectsWrongPlatform(t *testing.T) {
	j := job("Get-Disk", 30)
	j.Interpreter = "powershell"

	res := runJob(context.Background(), j)
	if runtime.GOOS == "windows" {
		return // there, powershell is the correct interpreter
	}
	if !strings.Contains(res.Error, "only run on windows") {
		t.Errorf("Error = %q, want a platform rejection", res.Error)
	}
}

func TestCapWriterTruncates(t *testing.T) {
	w := &capWriter{limit: 10}

	n, err := w.Write([]byte("12345"))
	if n != 5 || err != nil {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if w.truncated {
		t.Error("truncated set before the limit was reached")
	}

	// Writes must always report full length so the child process never sees a
	// short write, even once we have stopped recording.
	n, err = w.Write([]byte("678901234567890"))
	if n != 15 || err != nil {
		t.Fatalf("Write = (%d, %v), want (15, nil)", n, err)
	}
	if !w.truncated {
		t.Error("truncated not set after overflowing the limit")
	}
	if got := w.String(); got != "1234567890" {
		t.Errorf("String() = %q, want %q", got, "1234567890")
	}

	// Further writes past the limit stay discarded.
	w.Write([]byte("more"))
	if got := w.String(); got != "1234567890" {
		t.Errorf("String() = %q after extra write, want unchanged", got)
	}
}

func TestRunJobLargeOutputIsTruncated(t *testing.T) {
	skipOnWindows(t)

	// Produce well over the 64 KB cap.
	res := runJob(context.Background(), job("for i in $(seq 1 20000); do echo aaaaaaaaaaaaaaaaaaaa; done", 60))
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !res.Truncated {
		t.Error("truncated flag not set for oversized output")
	}
	if len(res.Stdout) > maxOutput {
		t.Errorf("stdout is %d bytes, want at most %d", len(res.Stdout), maxOutput)
	}
}
