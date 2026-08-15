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

// publicIP returns the cached address, resolving it when the cache is stale.
// Failure is not an error worth surfacing: the field is informational, and a
// heartbeat must never be held up by an unreachable third party.
func (a *Agent) publicIP(ctx context.Context) string {
	a.mu.Lock()
	cached, checked := a.cachedPublicIP, a.publicIPChecked
	a.mu.Unlock()

	if cached != "" && time.Since(checked) < publicIPRefresh {
		return cached
	}
	if a.cfg.NoPublicIP {
		return ""
	}

	resolved := resolvePublicIP(ctx, a.client)

	a.mu.Lock()
	// Keep the previous answer when resolution fails, so a transient outage
	// does not blank the address on the dashboard.
	if resolved != "" {
		a.cachedPublicIP = resolved
	}
	a.publicIPChecked = time.Now()
	cached = a.cachedPublicIP
	a.mu.Unlock()

	if resolved != "" && resolved != cached {
		a.log.Info("public address resolved", "ip", resolved)
	}
	return cached
}

// resolvePublicIP asks each service in turn until one answers with something
// that parses as an IP address.
func resolvePublicIP(ctx context.Context, client *http.Client) string {
	// Short per-service timeout: three unreachable services must not add up to
	// anything close to the check-in interval.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for _, url := range publicIPServices {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		// ifconfig.me returns HTML to browsers and plain text to curl.
		req.Header.Set("User-Agent", "curl/8")

		res, err := client.Do(req)
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
