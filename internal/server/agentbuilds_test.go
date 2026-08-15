package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ollie/monitorrr/internal/store"
)

func writeBuild(t *testing.T, dir, goos, arch, contents string) string {
	t.Helper()
	path := filepath.Join(dir, binaryName(goos, arch))
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write build: %v", err)
	}
	return path
}

func TestBuildCacheHashesAndInvalidates(t *testing.T) {
	dir := t.TempDir()
	writeBuild(t, dir, "linux", "amd64", "version one")

	c := newBuildCache()
	first, err := c.lookup(dir, "linux", "amd64")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if first.SHA256 == "" {
		t.Fatal("no digest computed")
	}

	// A repeat lookup is served from cache and must agree.
	again, err := c.lookup(dir, "linux", "amd64")
	if err != nil || again.SHA256 != first.SHA256 {
		t.Fatalf("cached lookup = (%+v, %v), want the same digest", again, err)
	}

	// A rebuild must be picked up without restarting the server.
	time.Sleep(10 * time.Millisecond)
	path := writeBuild(t, dir, "linux", "amd64", "version two, a different length")
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	rebuilt, err := c.lookup(dir, "linux", "amd64")
	if err != nil {
		t.Fatalf("lookup after rebuild: %v", err)
	}
	if rebuilt.SHA256 == first.SHA256 {
		t.Error("digest unchanged after the binary was rebuilt — the cache is stale")
	}

	if _, err := c.lookup(dir, "plan9", "386"); err == nil {
		t.Error("lookup of a platform with no build: want an error")
	}
}

func TestBinaryNameMatchesTheMakefile(t *testing.T) {
	cases := map[string]string{
		"linux/amd64":   "monitorrr-agent-linux-amd64",
		"darwin/arm64":  "monitorrr-agent-darwin-arm64",
		"windows/amd64": "monitorrr-agent-windows-amd64.exe",
		"windows/arm64": "monitorrr-agent-windows-arm64.exe",
	}
	for platform, want := range cases {
		goos, arch, _ := splitPlatform(platform)
		if got := binaryName(goos, arch); got != want {
			t.Errorf("binaryName(%s) = %q, want %q", platform, got, want)
		}
	}
}

func splitPlatform(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// The update decision is digest-based precisely so it cannot loop: once the
// agent runs the served bytes, there is nothing left to offer.
func TestUpdateForComparesDigestsNotVersions(t *testing.T) {
	dir := t.TempDir()
	writeBuild(t, dir, "linux", "amd64", "the served binary")

	s := &Server{cfg: Config{DistDir: dir}, builds: newBuildCache()}
	build, err := s.builds.lookup(dir, "linux", "amd64")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	device := store.Device{OS: "linux", Arch: "amd64"}

	t.Run("different digest offers an update", func(t *testing.T) {
		if u := s.updateFor(device, "some-other-digest"); u == nil {
			t.Fatal("no update offered to an agent running different bytes")
		} else if u.SHA256 != build.SHA256 {
			t.Errorf("offered digest = %s, want the served one %s", u.SHA256, build.SHA256)
		}
	})

	t.Run("matching digest offers nothing", func(t *testing.T) {
		if u := s.updateFor(device, build.SHA256); u != nil {
			t.Errorf("update offered to an agent already running the served bytes: %+v", u)
		}
	})

	t.Run("an agent that cannot report its digest is left alone", func(t *testing.T) {
		// Empty means "old agent" or "could not hash myself". Neither is a safe
		// basis for replacing a binary.
		if u := s.updateFor(device, ""); u != nil {
			t.Errorf("update offered without a digest to compare: %+v", u)
		}
	})

	t.Run("a platform with no build offers nothing", func(t *testing.T) {
		if u := s.updateFor(store.Device{OS: "windows", Arch: "arm64"}, "anything"); u != nil {
			t.Errorf("update offered for a platform this server has no build for: %+v", u)
		}
	})
}
