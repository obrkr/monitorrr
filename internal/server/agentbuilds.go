package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ollie/monitorrr/internal/proto"
	"github.com/ollie/monitorrr/internal/store"
)

// agentBuild is one platform's agent binary as it exists on disk right now.
type agentBuild struct {
	Path    string
	SHA256  string
	Size    int64
	ModTime time.Time
}

// buildCache avoids re-hashing multi-megabyte binaries on every heartbeat.
// Entries are invalidated by modification time, so a rebuild is picked up
// without restarting the server.
type buildCache struct {
	mu     sync.Mutex
	builds map[string]agentBuild
}

func newBuildCache() *buildCache {
	return &buildCache{builds: map[string]agentBuild{}}
}

// binaryName is the file `make build-all` produces for a platform.
func binaryName(goos, arch string) string {
	name := fmt.Sprintf("monitorrr-agent-%s-%s", goos, arch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// lookup returns the current build for a platform, hashing it if the file has
// changed since last time.
func (c *buildCache) lookup(distDir, goos, arch string) (agentBuild, error) {
	path := filepath.Join(distDir, binaryName(goos, arch))

	info, err := os.Stat(path)
	if err != nil {
		return agentBuild{}, err
	}

	key := goos + "/" + arch
	c.mu.Lock()
	cached, ok := c.builds[key]
	c.mu.Unlock()

	if ok && cached.ModTime.Equal(info.ModTime()) && cached.Size == info.Size() {
		return cached, nil
	}

	digest, err := hashFile(path)
	if err != nil {
		return agentBuild{}, err
	}
	build := agentBuild{Path: path, SHA256: digest, Size: info.Size(), ModTime: info.ModTime()}

	c.mu.Lock()
	c.builds[key] = build
	c.mu.Unlock()
	return build, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// updateFor decides whether a device should replace its own binary.
//
// The comparison is between the digest the agent reported for its own
// executable and the digest of the binary this server would serve it. Version
// strings are deliberately not used: they are opaque, can repeat across
// rebuilds, and an agent that updated to a build reporting the same version
// would be told to update again forever. Digests cannot loop — once the agent
// is running the served bytes, they match.
func (s *Server) updateFor(device store.Device, reportedSHA string) *proto.AgentUpdate {
	if reportedSHA == "" {
		// An agent too old to report its digest, or one that could not read its
		// own executable. Either way there is nothing safe to compare.
		return nil
	}

	build, err := s.builds.lookup(s.cfg.DistDir, device.OS, device.Arch)
	if err != nil {
		// No build for this platform is normal — not every fleet builds every
		// target — so this is not worth logging on every heartbeat.
		return nil
	}
	if build.SHA256 == reportedSHA {
		return nil
	}

	return &proto.AgentUpdate{
		SHA256:  build.SHA256,
		Size:    build.Size,
		Version: FullVersion(),
	}
}

// handleAgentBinary serves the agent its replacement binary.
//
// Which file to send is decided from the device's enrolled platform rather than
// anything the request says, so an agent cannot ask for a build for a platform
// that is not its own.
func (s *Server) handleAgentBinary(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}

	device, err := s.st.GetDevice(r.Context(), deviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "unknown device"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read device", err)
		return
	}

	build, err := s.builds.lookup(s.cfg.DistDir, device.OS, device.Arch)
	if err != nil {
		writeJSON(w, http.StatusNotFound, proto.Error{
			Error: fmt.Sprintf("no agent build for %s/%s on this server", device.OS, device.Arch)})
		return
	}

	f, err := os.Open(build.Path)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not open the agent build", err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not stat the agent build", err)
		return
	}

	s.log.Info("serving agent binary", "device", deviceID,
		"platform", device.OS+"/"+device.Arch, "sha256", build.SHA256, "bytes", build.Size)

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(build.Path), info.ModTime(), f)
}

// handleSetAutoUpdate turns fleet-wide auto-update on or off.
func (s *Server) handleSetAutoUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.st.SetAutoUpdate(r.Context(), body.Enabled); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not change the setting", err)
		return
	}
	s.log.Info("auto-update setting changed", "enabled", body.Enabled, "by", adminUser(r))
	writeJSON(w, http.StatusOK, map[string]any{"enabled": body.Enabled})
}
