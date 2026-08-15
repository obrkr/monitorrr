// Package server exposes the agent API, the JSON admin API, and the web UI.
//
// The agent endpoints (/v1/*) authenticate with per-device tokens. Everything
// else is admin surface, optionally protected by basic auth.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/ollie/monitorrr/internal/proto"
	"github.com/ollie/monitorrr/internal/store"
)

//go:embed web
var webFS embed.FS

// Version and BuildTime are stamped at build time via -ldflags.
var (
	Version   = "dev"
	BuildTime = "unknown"
)

// FullVersion identifies this exact build, so two builds of the same commit
// remain distinguishable.
func FullVersion() string {
	if BuildTime == "unknown" {
		return Version
	}
	return Version + " (" + BuildTime + ")"
}

// offlineGrace is how many missed check-ins before a device is called offline.
const offlineGrace = 3

// Config holds server runtime options.
type Config struct {
	Addr          string // listen address, e.g. ":8080"
	DBPath        string // path to the SQLite file
	DistDir       string // directory holding built agent binaries
	AdminPassword string // if set, the UI and admin API require basic auth
	PublicURL     string // base URL agents should call; inferred from request when empty
	TLSCert       string
	TLSKey        string
}

// Server wires the store, templates, and HTTP routes together.
type Server struct {
	cfg  Config
	st   *store.Store
	tmpl *template.Template
	// shTmpl is the installer script. It uses text/template deliberately:
	// html/template would escape shell operators into entities.
	shTmpl *texttemplate.Template
	log    *slog.Logger
	http   *http.Server
	// lanAddr is this host's outbound address, resolved once at startup and
	// used when a request arrives on loopback — see publicURL.
	lanAddr string
}

// New builds a Server. The caller owns the store's lifetime.
func New(cfg Config, st *store.Store, log *slog.Logger) (*Server, error) {
	tmpl, err := template.ParseFS(webFS, "web/templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	shTmpl, err := texttemplate.ParseFS(webFS, "web/install.sh.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse installer template: %w", err)
	}
	s := &Server{cfg: cfg, st: st, tmpl: tmpl, shTmpl: shTmpl, log: log, lanAddr: detectLANAddr()}
	if s.lanAddr != "" {
		log.Info("detected own network address", "addr", s.lanAddr,
			"hint", "used in install commands when the dashboard is opened on localhost")
	} else {
		log.Warn("could not determine this host's network address; " +
			"install commands may show localhost — pass -public-url to set it explicitly")
	}
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Agent API — token authenticated, no admin auth.
	mux.HandleFunc("POST /v1/enroll", s.handleEnroll)
	mux.HandleFunc("POST /v1/checkin", s.handleCheckin)
	mux.HandleFunc("POST /v1/jobs/{id}/result", s.handleJobResult)
	mux.HandleFunc("POST /v1/retire/ack", s.handleRetireAck)
	mux.HandleFunc("GET /v1/payload/{job}", s.handleServePayload)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	// Installer and binary downloads authenticate with the enrollment token
	// rather than admin credentials: an endpoint being provisioned has the
	// token but no business holding the admin password, and anyone with the
	// token can enroll anyway, so the trust level is the same.
	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	mux.HandleFunc("GET /download/{name}", s.handleDownload)

	// Static assets are public so the login prompt renders cleanly.
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(fmt.Sprintf("static assets missing: %v", err))
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	// Admin surface.
	admin := http.NewServeMux()
	admin.HandleFunc("GET /{$}", s.handleDashboard)
	admin.HandleFunc("GET /devices/{id}", s.handleDevicePage)
	admin.HandleFunc("GET /api/devices/{id}", s.handleGetDevice)
	admin.HandleFunc("POST /api/devices/{id}/dispatch", s.handleDeviceDispatch)
	admin.HandleFunc("GET /scripts", s.handleScriptsPage)
	admin.HandleFunc("GET /runs", s.handleRunsPage)
	admin.HandleFunc("GET /deployment", s.handleDeployment)
	admin.HandleFunc("GET /api/devices", s.handleListDevices)
	admin.HandleFunc("GET /api/events", s.handleListEvents)
	admin.HandleFunc("DELETE /api/devices/{id}", s.handleDeleteDevice)
	admin.HandleFunc("POST /api/devices/{id}/retire", s.handleRetireDevice)
	admin.HandleFunc("POST /api/devices/{id}/tags", s.handleSetTags)
	admin.HandleFunc("GET /api/tags", s.handleListTags)
	admin.HandleFunc("POST /api/dispatch", s.handleFleetDispatch)
	admin.HandleFunc("POST /api/settings/interval", s.handleSetInterval)
	admin.HandleFunc("POST /api/settings/rotate-token", s.handleRotateToken)
	admin.HandleFunc("GET /api/scripts", s.handleListScripts)
	admin.HandleFunc("POST /api/scripts", s.handleSaveScript)
	admin.HandleFunc("DELETE /api/scripts/{id}", s.handleDeleteScript)
	admin.HandleFunc("POST /api/scripts/{id}/dispatch", s.handleDispatch)
	admin.HandleFunc("GET /api/payloads", s.handleListPayloads)
	admin.HandleFunc("POST /api/payloads", s.handleUploadPayload)
	admin.HandleFunc("DELETE /api/payloads/{id}", s.handleDeletePayload)
	admin.HandleFunc("POST /api/scripts/{id}/payload", s.handleAttachPayload)
	admin.HandleFunc("GET /api/jobs", s.handleListJobs)
	admin.HandleFunc("GET /api/jobs/{id}", s.handleGetJob)
	mux.Handle("/", s.requireAdmin(admin))

	return s.withLogging(mux)
}

// Run starts the HTTP server and the offline sweeper, returning when ctx ends.
func (s *Server) Run(ctx context.Context) error {
	go s.sweepLoop(ctx)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
			s.log.Info("listening with TLS", "addr", s.cfg.Addr)
			err = s.http.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
		} else {
			s.log.Info("listening", "addr", s.cfg.Addr)
			err = s.http.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	}
}

// sweepLoop periodically marks silent devices offline. It ticks at a fraction
// of the check-in interval so the dashboard reacts promptly after the grace
// period elapses rather than at the next full interval boundary.
func (s *Server) sweepLoop(ctx context.Context) {
	tick := func() time.Duration {
		interval, err := s.st.DefaultCheckinInterval(ctx)
		if err != nil || interval <= 0 {
			interval = store.DefaultInterval
		}
		d := time.Duration(interval) * time.Second / 2
		if d < 5*time.Second {
			d = 5 * time.Second
		}
		return d
	}

	timer := time.NewTimer(tick())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			n, err := s.st.SweepOffline(ctx, offlineGrace)
			if err != nil {
				s.log.Error("offline sweep failed", "error", err)
			} else if n > 0 {
				s.log.Info("marked devices offline", "count", n)
			}

			// A job whose agent never reported back would otherwise sit in
			// "running" forever and misrepresent the fleet's real state.
			lost, err := s.st.SweepLostJobs(ctx)
			if err != nil {
				s.log.Error("lost-job sweep failed", "error", err)
			} else if lost > 0 {
				s.log.Warn("marked jobs lost", "count", lost)
			}
			timer.Reset(tick())
		}
	}
}

// --- agent API ---

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req proto.EnrollRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	want, err := s.st.EnrollToken(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "enrollment unavailable", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.EnrollToken), []byte(want)) != 1 {
		s.log.Warn("enrollment rejected", "remote", clientIP(r), "hostname", req.Hostname)
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "invalid enrollment token"})
		return
	}
	if req.Hostname == "" || req.OS == "" || req.Arch == "" {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: "hostname, os and arch are required"})
		return
	}

	id, token, err := s.st.Enroll(r.Context(), req.Hostname, req.OS, req.Arch, req.AgentVersion, clientIP(r))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "enrollment failed", err)
		return
	}
	interval, err := s.st.DefaultCheckinInterval(r.Context())
	if err != nil {
		interval = store.DefaultInterval
	}

	s.log.Info("device enrolled", "id", id, "hostname", req.Hostname, "os", req.OS, "arch", req.Arch)
	writeJSON(w, http.StatusOK, proto.EnrollResponse{AgentID: id, AgentToken: token, Interval: interval})
}

func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	id, token, ok := agentCredentials(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "missing agent credentials"})
		return
	}
	if err := s.st.Authenticate(r.Context(), id, token); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Also covers a device deleted from the dashboard: the agent sees
			// 401 and re-enrolls, which is the intended reset path.
			writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "unknown device or bad token"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "authentication failed", err)
		return
	}

	var req proto.CheckinRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	interval, retire, err := s.st.Checkin(r.Context(), id, req.Hostname, req.AgentVersion,
		clientIP(r), req.PublicIP, req.LocalIPs, req.Features)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "unknown device"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "check-in failed", err)
		return
	}

	// A retiring device is told to uninstall and nothing else — sending it work
	// it will never finish would just produce lost jobs.
	if retire {
		if !slices.Contains(req.Features, proto.FeatureRetire) {
			// The instruction goes out regardless — it costs nothing and a
			// future upgrade will act on it — but say plainly why the device is
			// stuck, rather than leaving it in "Retiring…" with no explanation.
			s.log.Warn("agent does not support retirement; upgrade it to complete decommissioning",
				"device", id, "hostname", req.Hostname, "agent_version", req.AgentVersion)
		} else {
			s.log.Info("instructing agent to retire", "device", id, "hostname", req.Hostname)
		}
		writeJSON(w, http.StatusOK, proto.CheckinResponse{Interval: interval, Retire: true})
		return
	}

	// Claiming marks the jobs running, so each one is handed out exactly once.
	claimed, err := s.st.ClaimJobs(r.Context(), id)
	if err != nil {
		// The heartbeat itself succeeded; failing it over a job-queue error
		// would take the device offline on the dashboard for no good reason.
		s.log.Error("could not claim jobs", "device", id, "error", err)
	}

	jobs := make([]proto.Job, 0, len(claimed))
	for _, j := range claimed {
		jobs = append(jobs, proto.Job{
			ID:          j.ID,
			Interpreter: j.Interpreter,
			Script:      j.Content,
			SHA256:      j.ScriptSHA256,
			TimeoutSecs: j.TimeoutSecs,
			// Without these the agent runs the script with no file, which fails
			// in a confusing way rather than not at all.
			PayloadName:   j.PayloadName,
			PayloadSHA256: j.PayloadSHA256,
		})
	}
	if len(jobs) > 0 {
		s.log.Info("dispatched jobs", "device", id, "count", len(jobs))
	}

	writeJSON(w, http.StatusOK, proto.CheckinResponse{Interval: interval, Jobs: jobs})
}

// --- admin API ---

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.st.ListDevices(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list devices", err)
		return
	}
	interval, err := s.st.DefaultCheckinInterval(r.Context())
	if err != nil {
		interval = store.DefaultInterval
	}

	online := 0
	for _, d := range devices {
		if d.Status == "online" {
			online++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":          devices,
		"online":           online,
		"total":            len(devices),
		"default_interval": interval,
		"server_time":      time.Now(),
	})
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.st.RecentEvents(r.Context(), limit)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list events", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.st.DeleteDevice(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such device"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not delete device", err)
		return
	}
	s.log.Info("device deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// handleRetireDevice asks a device to uninstall its agent. This is the
// counterpart to deleting a record: deleting leaves the agent running and it
// re-enrolls, whereas retiring removes the agent from the machine.
func (s *Server) handleRetireDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.st.RetireDevice(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such device"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not retire device", err)
		return
	}
	s.log.Info("device retirement requested", "id", id, "by", adminUser(r))
	writeJSON(w, http.StatusOK, map[string]any{"status": "retiring"})
}

// handleRetireAck records an agent's confirmation that it has uninstalled.
func (s *Server) handleRetireAck(w http.ResponseWriter, r *http.Request) {
	id, token, ok := agentCredentials(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "missing agent credentials"})
		return
	}
	if err := s.st.Authenticate(r.Context(), id, token); err != nil {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "unknown device or bad token"})
		return
	}
	if err := s.st.CompleteRetirement(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "device is not retiring"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not complete retirement", err)
		return
	}
	s.log.Info("device retired", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetInterval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seconds int `json:"seconds"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.st.SetDefaultCheckinInterval(r.Context(), body.Seconds); err != nil {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}
	s.log.Info("check-in interval changed", "seconds", body.Seconds)
	writeJSON(w, http.StatusOK, map[string]any{"seconds": body.Seconds})
}

func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	tok, err := s.st.RotateEnrollToken(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not rotate token", err)
		return
	}
	s.log.Info("enrollment token rotated")
	writeJSON(w, http.StatusOK, map[string]any{"enroll_token": tok})
}

// --- web UI ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "dashboard.html", map[string]any{
		"Page":    "dashboard",
		"Version": Version,
	})
}

func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request) {
	token, err := s.st.EnrollToken(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read enrollment token", err)
		return
	}
	interval, err := s.st.DefaultCheckinInterval(r.Context())
	if err != nil {
		interval = store.DefaultInterval
	}

	s.render(w, "deployment.html", map[string]any{
		"Page":        "deployment",
		"Version":     Version,
		"ServerURL":   s.publicURL(r),
		"EnrollToken": token,
		"Interval":    interval,
		"Builds":      s.availableBuilds(),
		"DistDir":     s.cfg.DistDir,
	})
}

// build describes one agent binary available for download.
type build struct {
	Name     string
	OS       string
	Arch     string
	Label    string
	SizeMB   string
	Modified string
}

var osLabels = map[string]string{
	"windows": "Windows",
	"darwin":  "macOS",
	"linux":   "Linux",
}

// availableBuilds lists agent binaries present in DistDir. Files are expected to
// be named monitorrr-agent-<os>-<arch>[.exe] as produced by `make build-all`.
func (s *Server) availableBuilds() []build {
	entries, err := os.ReadDir(s.cfg.DistDir)
	if err != nil {
		// Not an error worth failing the page over: the operator simply has not
		// run `make build-all` yet, and the template says so.
		return nil
	}

	var builds []build
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "monitorrr-agent-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		trimmed := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "monitorrr-agent-"), ".exe")
		parts := strings.SplitN(trimmed, "-", 2)
		if len(parts) != 2 {
			continue
		}
		label := osLabels[parts[0]]
		if label == "" {
			label = parts[0]
		}
		builds = append(builds, build{
			Name:     e.Name(),
			OS:       parts[0],
			Arch:     parts[1],
			Label:    fmt.Sprintf("%s / %s", label, parts[1]),
			SizeMB:   fmt.Sprintf("%.1f MB", float64(info.Size())/(1024*1024)),
			Modified: info.ModTime().Format("2006-01-02 15:04"),
		})
	}
	sort.Slice(builds, func(i, j int) bool { return builds[i].Name < builds[j].Name })
	return builds
}

// handleInstallScript renders a shell installer that detects the calling
// machine's OS and architecture. Picking the build by hand is the single
// easiest thing to get wrong when a lab mixes amd64 and arm64.
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	if !s.validEnrollToken(r) {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "valid ?token= required"})
		return
	}
	token, err := s.st.EnrollToken(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read enrollment token", err)
		return
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	if err := s.shTmpl.ExecuteTemplate(w, "install.sh.tmpl", map[string]any{
		"ServerURL":   s.publicURL(r),
		"EnrollToken": token,
	}); err != nil {
		s.log.Error("installer render failed", "error", err)
	}
}

// validEnrollToken checks the ?token= query parameter against the enrollment
// secret, in constant time.
func (s *Server) validEnrollToken(r *http.Request) bool {
	got := r.URL.Query().Get("token")
	if got == "" {
		return false
	}
	want, err := s.st.EnrollToken(r.Context())
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.validEnrollToken(r) {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "valid ?token= required"})
		return
	}
	name := r.PathValue("name")

	// Serve only files this server itself advertised, which rules out traversal
	// and stray files in the dist directory.
	var match string
	for _, b := range s.availableBuilds() {
		if b.Name == name {
			match = b.Name
			break
		}
	}
	if match == "" {
		http.NotFound(w, r)
		return
	}

	path := filepath.Join(s.cfg.DistDir, filepath.Base(match))
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read build", err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", match))
	http.ServeContent(w, r, match, info.ModTime(), f)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		// Headers are likely already flushed, so log rather than rewrite status.
		s.log.Error("template render failed", "template", name, "error", err)
	}
}

// publicURL is the base URL agents should be pointed at. An explicit
// -public-url wins; otherwise it is inferred from the request.
//
// The subtlety is localhost. Install commands are copied from this page and run
// on *other* machines, where "localhost" points back at themselves — so a
// browser on the server host would otherwise produce commands that silently
// fail everywhere else. When the request host is a loopback address, the
// server's own LAN address is substituted instead.
func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimSuffix(s.cfg.PublicURL, "/")
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}

	host := r.Host
	if hostname, port, err := net.SplitHostPort(host); err == nil {
		if isLoopback(hostname) {
			if lan := s.lanAddr; lan != "" {
				host = net.JoinHostPort(lan, port)
			}
		}
	} else if isLoopback(host) && s.lanAddr != "" {
		host = s.lanAddr
	}

	return fmt.Sprintf("%s://%s", scheme, host)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// detectLANAddr finds the address other machines on the network can reach this
// server on. No packet is actually sent: opening a UDP socket toward a public
// address makes the kernel pick the interface it would route through, which is
// the outbound address we want, and works without enumerating interfaces or
// guessing which of several is the real one.
func detectLANAddr() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsLoopback() {
		return ""
	}
	return addr.IP.String()
}

// --- middleware and helpers ---

// requireAdmin gates the admin surface behind basic auth when a password is
// configured. With no password set the UI is open, which is fine on an isolated
// lab network and warned about loudly at startup.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminPassword == "" {
			next.ServeHTTP(w, r)
			return
		}
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.AdminPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="monitorrr", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Heartbeats are the bulk of all traffic; logging each one at info level
		// would bury everything else.
		level := slog.LevelInfo
		if r.URL.Path == "/v1/checkin" && rec.status < 400 {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"remote", clientIP(r), "duration", time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// agentCredentials reads "Authorization: Bearer <agent-id>:<token>".
func agentCredentials(r *http.Request) (id, token string, ok bool) {
	h := r.Header.Get("Authorization")
	raw, found := strings.CutPrefix(h, "Bearer ")
	if !found {
		return "", "", false
	}
	id, token, found = strings.Cut(strings.TrimSpace(raw), ":")
	if !found || id == "" || token == "" {
		return "", "", false
	}
	return id, token, true
}

// clientIP prefers the reverse-proxy header when present, since a proxied
// deployment would otherwise record every device as coming from the proxy.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, found := strings.Cut(fwd, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: "malformed request body: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// fail logs the underlying cause and returns a generic message to the caller.
func (s *Server) fail(w http.ResponseWriter, status int, msg string, err error) {
	s.log.Error(msg, "error", err)
	writeJSON(w, status, proto.Error{Error: msg})
}
