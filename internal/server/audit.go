package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/ollie/monitorrr/internal/proto"
	"github.com/ollie/monitorrr/internal/store"
)

// auditNote lets a handler add detail to the entry the middleware will write.
type auditNote struct {
	action string
	target string
	detail string
	skip   bool
}

const auditKey ctxKey = "audit"

// withAuditNote attaches a mutable note to the request, filled in by handlers.
func withAuditNote(r *http.Request) (*http.Request, *auditNote) {
	note := &auditNote{}
	return r.WithContext(context.WithValue(r.Context(), auditKey, note)), note
}

// auditf records what a handler actually did, replacing the description the
// middleware would otherwise infer from the route.
func auditf(r *http.Request, action, target, detail string) {
	if note, ok := r.Context().Value(auditKey).(*auditNote); ok {
		note.action = action
		note.target = target
		note.detail = detail
	}
}

// auditSkip suppresses the entry for this request.
//
// Used by the factory reset, which empties the audit table as part of its work:
// the middleware writes afterwards, so without this the one surviving entry
// would be the reset itself, naming an account that no longer exists.
func auditSkip(r *http.Request) {
	if note, ok := r.Context().Value(auditKey).(*auditNote); ok {
		note.skip = true
	}
}

// recordChanges writes an audit entry for every successful state-changing
// request.
//
// This is middleware rather than a call in each handler for the same reason the
// read-only rule is: anything that changes state is a POST, PATCH or DELETE, so
// covering those covers everything by construction. A new endpoint is audited
// the moment it is added, not the moment someone remembers to instrument it.
func (s *Server) recordChanges(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}

		r, note := withAuditNote(r)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// A rejected request changed nothing; recording it as an action would
		// make the log describe things that never happened. Failed logins are
		// the exception and are recorded explicitly by their handler.
		if rec.status >= 400 || note.skip {
			return
		}

		user := userFrom(r)
		action, target := note.action, note.target
		if action == "" {
			action, target = describeRoute(r)
		}

		entry := store.AuditEntry{
			Username: user.Username,
			Role:     user.Role,
			Action:   action,
			Target:   target,
			Detail:   note.detail,
			Method:   r.Method,
			Path:     r.URL.Path,
			Status:   rec.status,
			IP:       clientIP(r),
		}
		if entry.Username == "" {
			entry.Username = "unauthenticated"
		}
		if err := s.st.AppendAudit(r.Context(), entry); err != nil {
			// Never fail the operation over its own bookkeeping.
			s.log.Error("could not write audit entry", "action", action, "error", err)
		}
	})
}

// describeRoute turns a method and path into a readable action, so an endpoint
// nobody has annotated still produces something meaningful.
func describeRoute(r *http.Request) (action, target string) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// Strip the api prefix; it says nothing about what happened.
	if len(parts) > 0 && parts[0] == "api" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return strings.ToLower(r.Method), ""
	}

	resource := singular(parts[0])
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodPost:
			return resource + ".create", ""
		case http.MethodDelete:
			return resource + ".delete", ""
		default:
			return resource + ".update", ""
		}
	case len(parts) == 2:
		// /scripts/{id}
		switch r.Method {
		case http.MethodDelete:
			return resource + ".delete", parts[1]
		case http.MethodPatch:
			return resource + ".update", parts[1]
		default:
			return resource + "." + parts[1], ""
		}
	default:
		// /devices/{id}/retire — the verb is the last segment.
		return resource + "." + parts[len(parts)-1], parts[1]
	}
}

func singular(s string) string {
	switch s {
	case "settings", "admin":
		return s
	}
	return strings.TrimSuffix(s, "s")
}

// --- audit page and API ---

func (s *Server) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "audit.html", s.pageData(r, "audit", map[string]any{}))
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)

	entries, err := s.st.ListAudit(r.Context(), store.AuditFilter{
		Username: q.Get("username"),
		Action:   q.Get("action"),
		Search:   q.Get("q"),
		Limit:    limit,
		Before:   before,
	})
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the audit log", err)
		return
	}

	actions, err := s.st.AuditActions(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read audit actions", err)
		return
	}
	total, err := s.st.AuditCount(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not count audit entries", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"actions": actions,
		"total":   total,
	})
}

// commonZones are offered as a shortlist in the settings menu. Any IANA name is
// accepted by the API — this is a convenience, not the set of valid answers.
var commonZones = []string{
	"UTC",
	"Europe/London", "Europe/Dublin", "Europe/Paris", "Europe/Berlin",
	"Europe/Madrid", "Europe/Rome", "Europe/Amsterdam", "Europe/Stockholm",
	"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
	"America/Toronto", "America/Sao_Paulo",
	"Asia/Dubai", "Asia/Kolkata", "Asia/Singapore", "Asia/Shanghai", "Asia/Tokyo",
	"Australia/Sydney", "Australia/Perth", "Pacific/Auckland",
}

func (s *Server) handleSetTheme(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Theme string `json:"theme"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.st.SetTheme(r.Context(), body.Theme); err != nil {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}
	auditf(r, "settings.theme", body.Theme, "display theme changed to "+body.Theme)
	s.log.Info("theme changed", "theme", body.Theme, "by", adminUser(r))
	writeJSON(w, http.StatusOK, map[string]any{"theme": body.Theme})
}

func (s *Server) handleSetTimezone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Timezone string `json:"timezone"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.st.SetTimezone(r.Context(), body.Timezone); err != nil {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}
	tz, _ := s.st.Timezone(r.Context())
	auditf(r, "settings.timezone", tz, "times are now displayed in "+tz)
	s.log.Info("timezone changed", "timezone", tz, "by", adminUser(r))
	writeJSON(w, http.StatusOK, map[string]any{"timezone": tz})
}
