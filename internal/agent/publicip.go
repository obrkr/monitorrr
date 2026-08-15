package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// publicIPRefresh is how often the address is re-resolved. A home lab's public
// address changes rarely, and this is an outbound call to a third party on every
// endpoint — doing it per heartbeat would be both wasteful and rude.
const publicIPRefresh = 30 * time.Minute

// publicIPServices are tried in order. Each returns the caller's address as
// bare text. Several are listed so one being down does not blind the fleet.
var publicIPServices = []string{
	"https://ifconfig.me/ip",
	"https://api.ipify.org",
	"https://icanhazip.com",
}

// publicIPClient is deliberately separate from the client used to talk to the
// monitorrr server. That one may have certificate verification disabled for a
// self-signed lab certificate (-insecure), and that decision must not silently
// extend to third-party services on the public internet.
var publicIPClient = &http.Client{Timeout: 10 * time.Second}

// publicIP returns the last known address, refreshing in the background when
// stale. It never blocks: resolution talks to a third party that may be slow or
// unreachable, and a heartbeat is not the place to wait for that. The cost is
// that a freshly started agent reports no address on its first check-in, which
// the server treats as "no change" rather than blanking the stored value.
func (a *Agent) publicIP() string {
	if a.cfg.NoPublicIP {
		return ""
	}

	a.mu.Lock()
	cached := a.cachedPublicIP
	stale := time.Since(a.publicIPChecked) >= publicIPRefresh
	if stale && !a.publicIPRefreshing {
		a.publicIPRefreshing = true
		go a.refreshPublicIP()
	}
	a.mu.Unlock()

	return cached
}

// refreshPublicIP resolves the address and updates the cache.
func (a *Agent) refreshPublicIP() {
	resolved := resolvePublicIP(context.Background())

	a.mu.Lock()
	changed := resolved != "" && resolved != a.cachedPublicIP
	// Keep the previous answer when resolution fails, so a transient outage
	// does not blank the address on the dashboard.
	if resolved != "" {
		a.cachedPublicIP = resolved
	}
	// Stamp the attempt either way, so an unreachable service is retried on the
	// normal schedule rather than on every single heartbeat.
	a.publicIPChecked = time.Now()
	a.publicIPRefreshing = false
	a.mu.Unlock()

	if changed {
		a.log.Info("public address resolved", "ip", resolved)
	}
}

// resolvePublicIP asks each service in turn until one answers with something
// that parses as an IP address.
func resolvePublicIP(ctx context.Context) string {
	// One budget for all attempts: three unreachable services must not add up
	// to anything close to a check-in interval.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for _, url := range publicIPServices {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		// ifconfig.me returns HTML to browsers and plain text to curl.
		req.Header.Set("User-Agent", "curl/8")

		res, err := publicIPClient.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 128))
		res.Body.Close()
		if err != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
			continue
		}
		if ip := net.ParseIP(strings.TrimSpace(string(body))); ip != nil {
			return ip.String()
		}
	}
	return ""
}
