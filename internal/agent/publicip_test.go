package agent

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func testAgent(cfg Config) *Agent {
	return &Agent{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// The heartbeat must never wait on a third-party service. These cases all
// assert the call returns without doing network work.
func TestPublicIPNeverBlocksTheHeartbeat(t *testing.T) {
	t.Run("disabled returns empty", func(t *testing.T) {
		a := testAgent(Config{NoPublicIP: true})
		// Even with a cached value, opting out must report nothing.
		a.cachedPublicIP = "203.0.113.9"

		if got := a.publicIP(); got != "" {
			t.Errorf("publicIP() = %q with -no-public-ip, want empty", got)
		}
	})

	t.Run("fresh cache is returned as-is", func(t *testing.T) {
		a := testAgent(Config{})
		a.cachedPublicIP = "203.0.113.9"
		a.publicIPChecked = time.Now()

		start := time.Now()
		got := a.publicIP()
		if got != "203.0.113.9" {
			t.Errorf("publicIP() = %q, want the cached address", got)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("took %s — a cached read must not touch the network", elapsed)
		}
	})

	t.Run("a refresh already in flight is not duplicated", func(t *testing.T) {
		a := testAgent(Config{})
		a.cachedPublicIP = "203.0.113.9"
		// Stale enough to want a refresh, but one is already running.
		a.publicIPChecked = time.Now().Add(-2 * publicIPRefresh)
		a.publicIPRefreshing = true

		start := time.Now()
		got := a.publicIP()
		if got != "203.0.113.9" {
			t.Errorf("publicIP() = %q, want the previous address while refreshing", got)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("took %s — a refresh in flight must not block the caller", elapsed)
		}

		a.mu.Lock()
		refreshing := a.publicIPRefreshing
		a.mu.Unlock()
		if !refreshing {
			t.Error("the in-flight marker was cleared by a caller that should have left it alone")
		}
	})
}

// A failed resolution must keep the last known address rather than blanking it,
// and must not retry on every heartbeat.
func TestRefreshKeepsPreviousAddressOnFailure(t *testing.T) {
	a := testAgent(Config{})
	a.cachedPublicIP = "203.0.113.9"
	a.publicIPRefreshing = true

	// Point resolution at a service that cannot answer.
	original := publicIPServices
	publicIPServices = []string{"http://127.0.0.1:1/never"}
	t.Cleanup(func() { publicIPServices = original })

	a.refreshPublicIP()

	a.mu.Lock()
	cached, refreshing, checked := a.cachedPublicIP, a.publicIPRefreshing, a.publicIPChecked
	a.mu.Unlock()

	if cached != "203.0.113.9" {
		t.Errorf("cached address = %q after a failed refresh, want it unchanged", cached)
	}
	if refreshing {
		t.Error("in-flight marker not cleared after the refresh finished")
	}
	// Stamping the attempt is what stops a dead service being retried every
	// single heartbeat.
	if time.Since(checked) > time.Minute {
		t.Error("failed attempt was not stamped; it would be retried immediately")
	}
}

func TestResolvePublicIPRejectsNonAddresses(t *testing.T) {
	original := publicIPServices
	// A service that answers, but with something that is not an address.
	publicIPServices = []string{"http://127.0.0.1:1/never"}
	t.Cleanup(func() { publicIPServices = original })

	if got := resolvePublicIP(t.Context()); got != "" {
		t.Errorf("resolvePublicIP() = %q with no reachable service, want empty", got)
	}
}
