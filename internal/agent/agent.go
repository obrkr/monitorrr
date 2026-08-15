// Package agent implements the endpoint collector.
//
// The agent only ever makes outbound HTTPS calls, so it works behind NAT with
// no inbound firewall rules — the same machine reports fine from a lab VLAN or
// a laptop on a hotel network. Its durable identity lives in a small JSON state
// file so a restart does not create a duplicate device.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/ollie/monitorrr/internal/proto"
)

// Version and BuildTime are stamped at build time via -ldflags.
var (
	Version   = "dev"
	BuildTime = "unknown"
)

// FullVersion identifies this exact build. It is what the agent reports and
// what the dashboard shows, so two builds of the same commit are still
// distinguishable — which matters when you are checking whether a fleet has
// actually picked up a fix.
func FullVersion() string {
	if BuildTime == "unknown" {
		return Version
	}
	return Version + " (" + BuildTime + ")"
}

// Config holds agent runtime options.
type Config struct {
	ServerURL   string
	EnrollToken string
	StatePath   string // defaults to a per-OS system path
	Insecure    bool   // skip TLS verification (self-signed lab certs)
	Once        bool   // single check-in, then exit — useful for testing
	NoPublicIP  bool   // do not resolve the public address via a third party
}

// state is the durable identity persisted between runs.
type state struct {
	ServerURL  string `json:"server_url"`
	AgentID    string `json:"agent_id"`
	AgentToken string `json:"agent_token"`
}

// Agent runs the check-in loop.
type Agent struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client
	st     state
	// interval is server-controlled and may change on any check-in.
	interval time.Duration

	// Jobs execute on their own goroutine so a long-running script never
	// delays heartbeats — otherwise a five-minute script would make the device
	// look offline while it was doing exactly what it was told.
	jobs chan proto.Job
	mu   sync.Mutex
	// seen guards against executing the same job twice if a check-in response
	// is somehow redelivered.
	seen map[string]bool

	// The public address is cached between refreshes; see publicip.go.
	cachedPublicIP  string
	publicIPChecked time.Time
}

// New builds an Agent.
func New(cfg Config, log *slog.Logger) (*Agent, error) {
	if cfg.ServerURL == "" {
		return nil, errors.New("server URL is required (-server)")
	}
	if cfg.StatePath == "" {
		cfg.StatePath = DefaultStatePath()
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Agent{
		cfg:    cfg,
		log:    log,
		client: &http.Client{Timeout: 15 * time.Second, Transport: transport},
		// Replaced by the server's value at enrollment and on every check-in;
		// this is only what we use before the first successful exchange.
		interval: 60 * time.Second,
		jobs:     make(chan proto.Job, 64),
		seen:     make(map[string]bool),
	}, nil
}

// DefaultStatePath returns the per-OS location for the identity file. Each is
// the platform's conventional home for machine-scoped service state.
func DefaultStatePath() string {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "monitorrr", "agent.json")
	case "darwin":
		return "/Library/Application Support/monitorrr/agent.json"
	default:
		return "/var/lib/monitorrr/agent.json"
	}
}

// Run enrolls if needed, then checks in until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	// Checked before anything else: a retired machine must not come back as a
	// new device just because a supervisor restarted the process.
	if a.isTombstoned() {
		a.log.Warn("this machine was retired; not enrolling",
			"marker", tombstonePath(a.cfg.StatePath),
			"hint", "delete the marker or reinstall the agent to enrol again")
		return nil
	}

	if err := a.loadState(); err != nil {
		return err
	}

	// A state file pointing at a different server belongs to a previous
	// deployment; re-enrolling is more useful than failing every check-in.
	if a.st.AgentID != "" && a.st.ServerURL != a.cfg.ServerURL {
		a.log.Warn("server URL changed since enrollment, re-enrolling",
			"old", a.st.ServerURL, "new", a.cfg.ServerURL)
		a.st = state{}
	}

	if a.st.AgentID == "" {
		if err := a.enroll(ctx); err != nil {
			return err
		}
	} else {
		a.log.Info("using stored identity", "agent_id", a.st.AgentID, "state", a.cfg.StatePath)
	}

	// In one-shot mode the jobs are drained inline so the process does not exit
	// while work is still queued; otherwise a worker runs alongside the loop.
	if a.cfg.Once {
		if err := a.checkin(ctx); err != nil {
			if errors.Is(err, errRetired) {
				return nil
			}
			return err
		}
		return a.drainJobs(ctx)
	}
	go a.jobWorker(ctx)

	// Backoff applies only to transport failures; a healthy loop always runs at
	// the server-provided interval.
	backoff := time.Second
	for {
		err := a.checkin(ctx)
		switch {
		case err == nil:
			backoff = time.Second
		case errors.Is(err, errRetired):
			// Uninstalled itself; exiting is the whole point.
			return nil
		case errors.Is(err, errUnauthorized):
			a.log.Warn("server rejected our identity, re-enrolling")
			a.st = state{}
			if err := a.saveState(); err != nil {
				a.log.Error("could not clear state file", "error", err)
			}
			if err := a.enroll(ctx); err != nil {
				a.log.Error("re-enrollment failed", "error", err)
			}
		default:
			a.log.Error("check-in failed", "error", err, "retry_in", backoff)
		}

		wait := a.interval
		if err != nil && !errors.Is(err, errUnauthorized) {
			wait = backoff
			if backoff < 5*time.Minute {
				backoff *= 2
			}
		}
		// Jitter keeps a fleet that booted together from synchronising into a
		// thundering herd against the server.
		wait += time.Duration(rand.Int63n(int64(wait/10 + 1)))

		select {
		case <-ctx.Done():
			a.log.Info("shutting down")
			return nil
		case <-time.After(wait):
		}
	}
}

var (
	errUnauthorized = errors.New("unauthorized")
	// errRetired ends the run loop cleanly after a self-uninstall.
	errRetired = errors.New("retired")
)

func (a *Agent) enroll(ctx context.Context) error {
	if a.cfg.EnrollToken == "" {
		return errors.New("no stored identity and no enrollment token supplied (-enroll-token)")
	}

	hostname, _ := os.Hostname()
	req := proto.EnrollRequest{
		EnrollToken:  a.cfg.EnrollToken,
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		AgentVersion: FullVersion(),
	}

	var resp proto.EnrollResponse
	if err := a.post(ctx, "/v1/enroll", "", req, &resp); err != nil {
		return fmt.Errorf("enroll: %w", err)
	}

	a.st = state{ServerURL: a.cfg.ServerURL, AgentID: resp.AgentID, AgentToken: resp.AgentToken}
	if err := a.saveState(); err != nil {
		return err
	}
	a.setInterval(resp.Interval)
	a.log.Info("enrolled", "agent_id", resp.AgentID, "interval", a.interval, "state", a.cfg.StatePath)
	return nil
}

func (a *Agent) checkin(ctx context.Context) error {
	hostname, _ := os.Hostname()
	req := proto.CheckinRequest{
		Hostname:     hostname,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		AgentVersion: FullVersion(),
		LocalIPs:     localIPs(),
		PublicIP:     a.publicIP(ctx),
		Features:     proto.AgentFeatures,
	}

	var resp proto.CheckinResponse
	auth := a.st.AgentID + ":" + a.st.AgentToken
	if err := a.post(ctx, "/v1/checkin", auth, req, &resp); err != nil {
		return err
	}

	if resp.Retire {
		a.retire(ctx)
		return errRetired
	}

	if resp.Interval > 0 && time.Duration(resp.Interval)*time.Second != a.interval {
		a.log.Info("check-in interval updated by server", "seconds", resp.Interval)
		a.setInterval(resp.Interval)
	}
	a.enqueue(ctx, resp.Jobs)
	a.log.Debug("checked in", "interval", a.interval, "jobs", len(resp.Jobs))
	return nil
}

// enqueue hands new jobs to the worker. Queueing happens off the check-in path
// so a backlog can never stall the heartbeat.
func (a *Agent) enqueue(ctx context.Context, jobs []proto.Job) {
	for _, job := range jobs {
		a.mu.Lock()
		duplicate := a.seen[job.ID]
		if !duplicate {
			a.seen[job.ID] = true
		}
		a.mu.Unlock()
		if duplicate {
			continue
		}

		a.log.Info("job received", "job", job.ID, "interpreter", job.Interpreter, "timeout", job.TimeoutSecs)
		go func(job proto.Job) {
			select {
			case a.jobs <- job:
			case <-ctx.Done():
			}
		}(job)
	}
}

// drainJobs runs everything currently queued, then returns. The grace period
// covers the brief gap between a check-in accepting jobs and them arriving on
// the channel.
func (a *Agent) drainJobs(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case job := <-a.jobs:
			result := runJob(ctx, job)
			a.log.Info("job finished", "job", job.ID, "exit", result.ExitCode, "ms", result.DurationMS)
			a.reportResult(ctx, job.ID, result)
		case <-time.After(250 * time.Millisecond):
			return nil
		}
	}
}

// jobWorker executes jobs one at a time. Serial execution is deliberate: two
// remediation scripts racing on the same endpoint is rarely what anyone wants.
func (a *Agent) jobWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-a.jobs:
			result := runJob(ctx, job)
			if result.Error != "" {
				a.log.Warn("job failed", "job", job.ID, "error", result.Error, "ms", result.DurationMS)
			} else {
				a.log.Info("job finished", "job", job.ID, "exit", result.ExitCode, "ms", result.DurationMS)
			}
			a.reportResult(ctx, job.ID, result)

			a.mu.Lock()
			delete(a.seen, job.ID)
			a.mu.Unlock()
		}
	}
}

// reportResult posts a finished job back, retrying briefly so a momentary
// network blip does not lose the output. If it still fails the server's own
// sweeper eventually marks the job lost.
func (a *Agent) reportResult(ctx context.Context, jobID string, result proto.JobResult) {
	auth := a.st.AgentID + ":" + a.st.AgentToken
	path := "/v1/jobs/" + jobID + "/result"

	backoff := time.Second
	for attempt := 1; attempt <= 4; attempt++ {
		err := a.post(ctx, path, auth, result, nil)
		if err == nil {
			return
		}
		if errors.Is(err, errUnauthorized) {
			a.log.Error("result rejected: this device is no longer enrolled", "job", jobID)
			return
		}
		a.log.Warn("could not report job result", "job", jobID, "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff *= 2
		}
	}
	a.log.Error("giving up reporting job result", "job", jobID)
}

func (a *Agent) setInterval(seconds int) {
	if seconds <= 0 {
		seconds = 60
	}
	a.interval = time.Duration(seconds) * time.Second
}

func (a *Agent) post(ctx context.Context, path, bearer string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ServerURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "monitorrr-agent/"+Version)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("contact server: %w", err)
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if res.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var e proto.Error
		if json.Unmarshal(payload, &e) == nil && e.Error != "" {
			return fmt.Errorf("server returned %d: %s", res.StatusCode, e.Error)
		}
		return fmt.Errorf("server returned %d", res.StatusCode)
	}
	// Not every endpoint answers with a body — a job result is acknowledged with
	// 204 No Content — so an empty payload is success, not a decode failure.
	if out == nil || len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (a *Agent) loadState() error {
	data, err := os.ReadFile(a.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // first run
	}
	if err != nil {
		return fmt.Errorf("read state file %s: %w", a.cfg.StatePath, err)
	}
	if err := json.Unmarshal(data, &a.st); err != nil {
		return fmt.Errorf("parse state file %s: %w", a.cfg.StatePath, err)
	}
	return nil
}

func (a *Agent) saveState() error {
	if err := os.MkdirAll(filepath.Dir(a.cfg.StatePath), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(a.st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	// The file holds this device's credential, so keep it owner-readable only.
	// Write to a temp file and rename so a crash cannot leave it truncated.
	tmp := a.cfg.StatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	if err := os.Rename(tmp, a.cfg.StatePath); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

// localIPs returns the machine's non-loopback addresses. Link-local addresses
// are skipped: they are noise on a dashboard and never routable to the server.
func localIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipnet.IP.String())
	}
	return out
}
