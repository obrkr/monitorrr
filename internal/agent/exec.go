package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// fetchPayload downloads a job's attached file to dest. It is a parameter
// rather than a hard dependency so execution can be tested without a server.
type fetchPayload func(ctx context.Context, jobID, dest string) error

// runJob executes one dispatched script and always returns a result — an
// execution that could not start is reported through JobResult.Error rather
// than as a Go error, because the server needs a record either way.
func runJob(ctx context.Context, job proto.Job, fetch fetchPayload) proto.JobResult {
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

	// A job may carry a file — an installer, a package — that the script acts
	// on. It is fetched into a private directory and handed over by path, so
	// the script decides what to do with it.
	var payloadPath string
	if job.PayloadName != "" {
		if fetch == nil {
			result.Error = "job has an attached file but this agent cannot fetch it"
			return finish()
		}
		var release func()
		payloadPath, release, err = downloadPayload(ctx, job, fetch)
		if err != nil {
			result.Error = err.Error()
			return finish()
		}
		defer release()
	}

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
	if payloadPath != "" {
		// Passed by environment rather than as an argument so the same variable
		// works identically for sh and PowerShell scripts.
		cmd.Env = append(os.Environ(),
			"MONITORRR_PAYLOAD="+payloadPath,
			"MONITORRR_PAYLOAD_NAME="+job.PayloadName)
	}

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

// utf8BOM is written ahead of every PowerShell script. Windows PowerShell 5.1
// decodes a .ps1 file with the system ANSI code page unless it starts with a
// byte-order mark, so a UTF-8 script containing anything outside ASCII arrives
// mangled: an em dash becomes three CP1252 characters, the last of which is a
// closing quote, and the script fails to parse for reasons nothing in it
// explains. PowerShell 7 reads UTF-8 either way and tolerates the mark.
//
// The mark is added on the way to disk only. Job checksums are computed over
// the script body the server sent, so this cannot affect verification.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// writeScript materialises the script in a private temp file.
func writeScript(job proto.Job) (path string, cleanup func(), err error) {
	ext, prefix := ".sh", []byte(nil)
	if job.Interpreter == "powershell" {
		ext, prefix = ".ps1", utf8BOM
	}

	f, err := os.CreateTemp("", "monitorrr-job-*"+ext)
	if err != nil {
		return "", nil, fmt.Errorf("create temp script: %w", err)
	}
	cleanup = func() { os.Remove(f.Name()) }

	if _, err := f.Write(prefix); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write temp script: %w", err)
	}
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

// downloadPayload fetches a job's file into a private directory and verifies it.
// The directory is removed when the returned function is called.
func downloadPayload(ctx context.Context, job proto.Job, fetch fetchPayload) (path string, release func(), err error) {
	dir, err := os.MkdirTemp("", "monitorrr-payload-*")
	if err != nil {
		return "", nil, fmt.Errorf("create payload directory: %w", err)
	}
	release = func() { os.RemoveAll(dir) }

	// Keep the original filename: installers frequently care about their own
	// extension, and a script referring to it reads better in the log.
	path = filepath.Join(dir, filepath.Base(job.PayloadName))
	if err := fetch(ctx, job.ID, path); err != nil {
		release()
		return "", nil, fmt.Errorf("download %s: %w", job.PayloadName, err)
	}

	sum, err := hashFile(path)
	if err != nil {
		release()
		return "", nil, err
	}
	if job.PayloadSHA256 != "" && sum != job.PayloadSHA256 {
		release()
		return "", nil, fmt.Errorf("payload checksum mismatch: server sent %s, computed %s",
			job.PayloadSHA256, sum)
	}

	// Executable: the pushed file is very often an installer meant to be run.
	if err := os.Chmod(path, 0o700); err != nil {
		release()
		return "", nil, fmt.Errorf("chmod payload: %w", err)
	}
	return path, release, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open payload: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash payload: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
