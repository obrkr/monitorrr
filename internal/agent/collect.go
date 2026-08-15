package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ollie/monitorrr/internal/proto"
)

// maxCollect caps a single collected file, matching the server's upload limit.
const maxCollect = 2 << 30 // 2 GiB

// collect fetches one requested file and sends it to the server.
//
// The size is reported before any bytes move. An operator who has asked for
// something enormous by mistake — a VM image, a swap file — finds out from the
// dashboard rather than after the transfer has run for an hour.
func (a *Agent) collect(ctx context.Context, req proto.Collect) {
	a.log.Info("collecting file", "collection", req.ID, "path", req.Path)

	source, copied, cleanup, err := a.openForCollection(req.Path)
	if err != nil {
		a.log.Warn("cannot collect file", "path", req.Path, "error", err)
		a.reportCollectMeta(ctx, req.ID, proto.CollectMeta{Error: err.Error()})
		return
	}
	defer cleanup()

	info, err := source.Stat()
	if err != nil {
		source.Close()
		a.reportCollectMeta(ctx, req.ID, proto.CollectMeta{Error: "cannot stat file: " + err.Error()})
		return
	}
	size := info.Size()

	if size > maxCollect {
		source.Close()
		a.reportCollectMeta(ctx, req.ID, proto.CollectMeta{
			Filename: filepath.Base(req.Path),
			Size:     size,
			Copied:   copied,
			Error:    fmt.Sprintf("file is %d bytes, over the %d byte limit", size, int64(maxCollect)),
		})
		return
	}

	if err := a.reportCollectMeta(ctx, req.ID, proto.CollectMeta{
		Filename: filepath.Base(req.Path),
		Size:     size,
		Copied:   copied,
	}); err != nil {
		source.Close()
		a.log.Error("could not report file size", "collection", req.ID, "error", err)
		return
	}

	if err := a.uploadCollection(ctx, req.ID, source, size); err != nil {
		source.Close()
		a.log.Error("file transfer failed", "collection", req.ID, "error", err)
		return
	}
	source.Close()
	a.log.Info("file collected", "collection", req.ID, "bytes", size, "copied", copied)
}

// openForCollection opens the requested file, falling back to a copy when the
// original cannot be read directly.
//
// A file held open exclusively — a running database, a log an application has
// locked, anything in use on Windows — cannot be opened for reading, but can
// very often still be copied by the OS. Copying aside and reading the copy is
// what makes those collectable. The temporary copy is removed by cleanup.
func (a *Agent) openForCollection(path string) (f *os.File, copied bool, cleanup func(), err error) {
	noop := func() {}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false, noop, fmt.Errorf("no such file: %s", path)
		}
		return nil, false, noop, fmt.Errorf("cannot access %s: %w", path, err)
	}

	// The straightforward case.
	f, openErr := os.Open(path)
	if openErr == nil {
		return f, false, noop, nil
	}
	a.log.Info("direct read failed, trying a copy", "path", path, "error", openErr)

	// Locked or permission-denied: duplicate it and read the duplicate.
	dir, err := os.MkdirTemp("", "monitorrr-collect-*")
	if err != nil {
		return nil, false, noop, fmt.Errorf("create temp directory: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }

	if err := copyAside(path, tmp(dir, path)); err != nil {
		cleanup()
		// Why it could not be read changes what the operator should do about
		// it, so the two are not reported as the same problem. A permissions
		// failure is fixed by how the agent is installed; a lock is not.
		if os.IsPermission(openErr) {
			return nil, false, noop, fmt.Errorf(
				"permission denied reading %s — the agent needs to run with more privilege (install it as a service so it runs as root or SYSTEM)", path)
		}
		return nil, false, noop, fmt.Errorf("file is in use and could not be copied: %w", err)
	}

	f, err = os.Open(tmp(dir, path))
	if err != nil {
		cleanup()
		return nil, false, noop, fmt.Errorf("cannot read the copy: %w", err)
	}
	return f, true, cleanup, nil
}

// tmp is where a copied-aside file lands, keeping its original name.
func tmp(dir, path string) string { return filepath.Join(dir, filepath.Base(path)) }

// copyAside duplicates a file using the platform's own copy, which can read
// some files that a plain open cannot.
func copyAside(src, dst string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Copy-Item handles more sharing situations than a naive open, and
		// -Force pushes past read-only attributes.
		cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
			"-Command", fmt.Sprintf("Copy-Item -LiteralPath %q -Destination %q -Force", src, dst))
	} else {
		cmd = exec.Command("cp", "-p", src, dst)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func (a *Agent) reportCollectMeta(ctx context.Context, id string, meta proto.CollectMeta) error {
	return a.post(ctx, "/v1/collect/"+id+"/meta", a.st.AgentID+":"+a.st.AgentToken, meta, nil)
}

// uploadCollection streams the file to the server.
//
// The body is the raw file rather than a multipart form: there is exactly one
// file, the server knows what it asked for, and streaming keeps memory flat
// regardless of size.
func (a *Agent) uploadCollection(ctx context.Context, id string, source io.Reader, size int64) error {
	url := a.cfg.ServerURL + "/v1/collect/" + id + "/data"

	hash := sha256.New()
	body := io.TeeReader(source, hash)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.st.AgentID+":"+a.st.AgentToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", "monitorrr-agent/"+Version)
	// Setting this lets the server show a progress bar rather than an unbounded
	// byte count, and lets it reject an oversized transfer before reading it.
	req.ContentLength = size

	// A large file over a slow link must not hit the timeout sized for small
	// JSON exchanges.
	client := *a.client
	client.Timeout = 0

	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 4096))

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("server returned %d", res.StatusCode)
	}

	// The digest is confirmed after the body has been read, so it covers
	// exactly the bytes that were sent.
	return a.post(ctx, "/v1/collect/"+id+"/complete", a.st.AgentID+":"+a.st.AgentToken,
		map[string]any{"sha256": hex.EncodeToString(hash.Sum(nil))}, nil)
}

// collectWorker runs file collections one at a time, off the heartbeat path.
func (a *Agent) collectWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-a.collections:
			a.collect(ctx, req)

			a.mu.Lock()
			delete(a.seenCollections, req.ID)
			a.mu.Unlock()
		}
	}
}

// enqueueCollections hands new requests to the worker without blocking the
// check-in that delivered them.
func (a *Agent) enqueueCollections(ctx context.Context, reqs []proto.Collect) {
	for _, req := range reqs {
		a.mu.Lock()
		duplicate := a.seenCollections[req.ID]
		if !duplicate {
			a.seenCollections[req.ID] = true
		}
		a.mu.Unlock()
		if duplicate {
			continue
		}

		go func(req proto.Collect) {
			select {
			case a.collections <- req:
			case <-ctx.Done():
			}
		}(req)
	}
}

// drainCollections runs everything currently queued, for one-shot mode.
func (a *Agent) drainCollections(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-a.collections:
			a.collect(ctx, req)
		case <-time.After(250 * time.Millisecond):
			return
		}
	}
}
