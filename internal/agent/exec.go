package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/ollie/monitorrr/internal/proto"
)

// maxOutput caps each captured stream. Truncating at the source keeps a
// runaway script from filling the network and the server's database.
const maxOutput = 64 * 1024

// runJob executes one dispatched script and always returns a result — an
// execution that could not start is reported through JobResult.Error rather
// than as a Go error, because the server needs a record either way.
func runJob(ctx context.Context, job proto.Job) proto.JobResult {
	start := time.Now()
	result := proto.JobResult{ExitCode: -1}
	finish := func() proto.JobResult {
		result.DurationMS = time.Since(start).Milliseconds()
		return result
	}

	// Verify before writing anything to disk: a payload that does not match the
	// hash the server sent is refused outright rather than partially executed.
	sum := sha256.Sum256([]byte(job.Script))
	if got := hex.EncodeToString(sum[:]); got != job.SHA256 {
		result.Error = fmt.Sprintf("checksum mismatch: server sent %s, computed %s", job.SHA256, got)
		return finish()
	}

	path, cleanup, err := writeScript(job)
	if err != nil {
		result.Error = err.Error()
		return finish()
	}
	defer cleanup()

	name, args, err := interpreterCommand(job.Interpreter, path)
	if err != nil {
		result.Error = err.Error()
		return finish()
	}

	timeout := time.Duration(job.TimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout := &capWriter{limit: maxOutput}
	stderr := &capWriter{limit: maxOutput}

	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Dir = os.TempDir()

	// A script is a process tree, not a process: `sh -c 'sleep 30'` leaves the
	// sleep running if only the shell is killed, and because that grandchild
	// inherits the output pipe, Wait would block until it finished anyway.
	// Run the job in its own process group and kill the group on timeout.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	// Backstop for a descendant that survives the signal and holds the pipe
	// open: after this, Wait gives up on the output and returns.
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	result.Truncated = stdout.truncated || stderr.truncated

	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		result.Error = fmt.Sprintf("timed out after %s", timeout)
	case runErr == nil:
		result.ExitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// A non-zero exit is a normal outcome, not an execution failure.
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Error = runErr.Error()
		}
	}
	return finish()
}

// writeScript materialises the script in a private temp file.
func writeScript(job proto.Job) (path string, cleanup func(), err error) {
	ext := ".sh"
	if job.Interpreter == "powershell" {
		ext = ".ps1"
	}

	f, err := os.CreateTemp("", "monitorrr-job-*"+ext)
	if err != nil {
		return "", nil, fmt.Errorf("create temp script: %w", err)
	}
	cleanup = func() { os.Remove(f.Name()) }

	if _, err := f.WriteString(job.Script); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write temp script: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temp script: %w", err)
	}
	// Owner-only: the script may carry credentials, and a world-readable file in
	// a shared temp directory would leak it to every local user.
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("chmod temp script: %w", err)
	}
	return f.Name(), cleanup, nil
}

// interpreterCommand maps an interpreter to an executable and arguments.
func interpreterCommand(interpreter, path string) (string, []string, error) {
	switch interpreter {
	case "sh":
		if runtime.GOOS == "windows" {
			return "", nil, errors.New("shell scripts cannot run on windows")
		}
		return "/bin/sh", []string{path}, nil

	case "powershell":
		if runtime.GOOS != "windows" {
			return "", nil, errors.New("powershell scripts can only run on windows")
		}
		// Windows PowerShell first, then PowerShell 7+, which is what a modern
		// image may have instead.
		for _, candidate := range []string{"powershell.exe", "pwsh.exe"} {
			if found, err := exec.LookPath(candidate); err == nil {
				return found, []string{
					"-NoProfile", "-NonInteractive",
					"-ExecutionPolicy", "Bypass",
					"-File", filepath.Clean(path),
				}, nil
			}
		}
		return "", nil, errors.New("neither powershell.exe nor pwsh.exe found on PATH")

	default:
		return "", nil, fmt.Errorf("unsupported interpreter %q", interpreter)
	}
}

// capWriter collects output up to a byte limit and notes whether it overflowed.
type capWriter struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Always report the full length written: the command should keep running
	// normally after we stop recording, not receive a short-write error.
	if remaining := w.limit - len(w.buf); remaining > 0 {
		if len(p) > remaining {
			w.buf = append(w.buf, p[:remaining]...)
			w.truncated = true
		} else {
			w.buf = append(w.buf, p...)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
