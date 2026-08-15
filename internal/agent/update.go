package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ollie/monitorrr/internal/proto"
)

// installPath is where the installer puts the agent. It is a variable rather
// than a constant so a deployment that installs elsewhere can set it at build
// time with -ldflags, and so tests can exercise self-replacement without
// touching a real installation.
var installPath = defaultInstallPath()

// ownBinarySHA256 returns the digest of the running executable, computed once.
//
// The binary cannot change underneath a running process in a way that matters:
// after an update we exit immediately, so a stale value can never be reported.
func (a *Agent) ownBinarySHA256() string {
	a.mu.Lock()
	cached := a.binarySHA
	computed := a.binaryHashed
	a.mu.Unlock()
	if computed {
		return cached
	}

	digest := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if d, err := hashFile(exe); err == nil {
			digest = d
		} else {
			a.log.Warn("could not hash own binary; auto-update is disabled for this agent", "error", err)
		}
	}

	a.mu.Lock()
	a.binarySHA = digest
	a.binaryHashed = true
	a.mu.Unlock()
	return digest
}

// selfUpdate replaces this agent's binary and exits so the supervisor restarts
// it. Every failure leaves the existing installation untouched.
func (a *Agent) selfUpdate(ctx context.Context, update *proto.AgentUpdate) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine own path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// Same rule as retirement: only a real installation is modified. Replacing
	// a developer's freshly built binary would be baffling.
	if exe != installPath {
		return fmt.Errorf("not updating: running from %s, not the install path %s", exe, installPath)
	}

	// Logged as the server's version rather than the replacement's: the server
	// reports what it is running, and the two only coincide because they are
	// built together. The digest below is the authoritative identity.
	a.log.Info("downloading agent update", "server_version", update.Version, "bytes", update.Size)

	// Staged beside the target so the final move is a rename within one
	// filesystem, which is atomic, rather than a copy across devices.
	dir := filepath.Dir(exe)
	staged := filepath.Join(dir, ".monitorrr-agent.new")
	defer os.Remove(staged)

	if err := a.downloadBinary(ctx, staged); err != nil {
		return err
	}

	digest, err := hashFile(staged)
	if err != nil {
		return fmt.Errorf("hash downloaded binary: %w", err)
	}
	if digest != update.SHA256 {
		return fmt.Errorf("checksum mismatch: server said %s, downloaded %s", update.SHA256, digest)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("chmod downloaded binary: %w", err)
	}

	// Run the replacement before trusting it. A truncated or wrong-architecture
	// binary that passes a checksum but cannot execute would otherwise brick
	// every machine it reached, and the fleet would have no way to receive the
	// fix — this is the one failure that must not be allowed to happen.
	if err := verifyRunnable(ctx, staged); err != nil {
		return fmt.Errorf("downloaded binary does not run, keeping the current one: %w", err)
	}

	if err := swapBinary(staged, exe); err != nil {
		return fmt.Errorf("install downloaded binary: %w", err)
	}

	a.log.Info("agent updated, restarting", "sha256", digest, "server_version", update.Version)
	return nil
}

// downloadBinary streams the replacement to dest.
func (a *Agent) downloadBinary(ctx context.Context, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.ServerURL+"/v1/agent/binary", nil)
	if err != nil {
		return fmt.Errorf("build update request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.st.AgentID+":"+a.st.AgentToken)
	req.Header.Set("User-Agent", "monitorrr-agent/"+Version)

	client := *a.client
	client.Timeout = 0
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch update: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d fetching the update", res.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create staged binary: %w", err)
	}
	if _, err := io.Copy(f, io.LimitReader(res.Body, maxPayload)); err != nil {
		f.Close()
		return fmt.Errorf("write staged binary: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("finish staged binary: %w", err)
	}
	return nil
}

// verifyRunnable checks the staged binary actually executes and identifies
// itself as a monitorrr agent.
func verifyRunnable(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "-version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "monitorrr-agent") {
		return fmt.Errorf("unexpected output from -version: %q", strings.TrimSpace(string(out)))
	}
	return nil
}
