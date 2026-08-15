package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
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

	res := runJob(context.Background(), job("echo out; echo err >&2", 30), nil)
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

	res := runJob(context.Background(), job("exit 42", 30), nil)
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

	res := runJob(context.Background(), tampered, nil)
	if !strings.Contains(res.Error, "checksum mismatch") {
		t.Errorf("Error = %q, want a checksum mismatch", res.Error)
	}
	if res.Stdout != "" {
		t.Errorf("tampered script produced output %q — it must not run at all", res.Stdout)
	}
}

func TestRunJobTimesOut(t *testing.T) {
	skipOnWindows(t)

	res := runJob(context.Background(), job("sleep 30", 1), nil)
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

	res := runJob(context.Background(), j, nil)
	if !strings.Contains(res.Error, "unsupported interpreter") {
		t.Errorf("Error = %q, want an unsupported-interpreter error", res.Error)
	}
}

func TestRunJobRejectsWrongPlatform(t *testing.T) {
	j := job("Get-Disk", 30)
	j.Interpreter = "powershell"

	res := runJob(context.Background(), j, nil)
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
	res := runJob(context.Background(), job("for i in $(seq 1 20000); do echo aaaaaaaaaaaaaaaaaaaa; done", 60), nil)
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

// A job carrying a file must have it fetched, verified, made executable, and
// handed to the script by path.
func TestRunJobDeliversPayloadToScript(t *testing.T) {
	skipOnWindows(t)

	contents := []byte("#!/bin/sh\necho installed\n")
	sum := sha256.Sum256(contents)

	j := job("test -x \"$MONITORRR_PAYLOAD\" && echo \"name=$MONITORRR_PAYLOAD_NAME\" && cat \"$MONITORRR_PAYLOAD\"", 30)
	j.PayloadName = "installer.sh"
	j.PayloadSHA256 = hex.EncodeToString(sum[:])

	fetched := ""
	fetch := func(ctx context.Context, jobID, dest string) error {
		fetched = dest
		return os.WriteFile(dest, contents, 0o600)
	}

	res := runJob(context.Background(), j, fetch)
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0. stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "name=installer.sh") {
		t.Errorf("stdout %q does not carry the original filename", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "installed") {
		t.Errorf("stdout %q does not contain the payload contents", res.Stdout)
	}
	// The download directory is temporary and must not survive the run.
	if fetched != "" {
		if _, err := os.Stat(filepath.Dir(fetched)); !os.IsNotExist(err) {
			t.Errorf("payload directory %s was left behind", filepath.Dir(fetched))
		}
	}
}

// A file that does not match the digest the server sent must not reach the
// script — this is the guard against a corrupted or substituted installer.
func TestRunJobRejectsPayloadChecksumMismatch(t *testing.T) {
	skipOnWindows(t)

	j := job("echo should not run", 30)
	j.PayloadName = "installer.sh"
	j.PayloadSHA256 = hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("expected")); return s[:] }())

	fetch := func(ctx context.Context, jobID, dest string) error {
		return os.WriteFile(dest, []byte("something else entirely"), 0o600)
	}

	res := runJob(context.Background(), j, fetch)
	if !strings.Contains(res.Error, "payload checksum mismatch") {
		t.Errorf("Error = %q, want a payload checksum mismatch", res.Error)
	}
	if res.Stdout != "" {
		t.Errorf("script produced output %q — it must not run at all", res.Stdout)
	}
}

// An agent too old to fetch payloads must fail the job loudly rather than
// running a script whose file never arrived.
func TestRunJobWithoutFetcherFailsClearly(t *testing.T) {
	j := job("echo hi", 30)
	j.PayloadName = "installer.msi"

	res := runJob(context.Background(), j, nil)
	if !strings.Contains(res.Error, "cannot fetch") {
		t.Errorf("Error = %q, want a clear inability-to-fetch error", res.Error)
	}
}

// A failed download must be reported, not silently treated as "no file".
func TestRunJobReportsDownloadFailure(t *testing.T) {
	j := job("echo hi", 30)
	j.PayloadName = "installer.msi"

	fetch := func(ctx context.Context, jobID, dest string) error {
		return errors.New("server returned 404")
	}

	res := runJob(context.Background(), j, fetch)
	if !strings.Contains(res.Error, "404") {
		t.Errorf("Error = %q, want the download failure surfaced", res.Error)
	}
}
