package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ollie/monitorrr/internal/proto"
	"github.com/ollie/monitorrr/internal/store"
)

// sessionCookie is the login cookie's name.
const sessionCookie = "monitorrr_session"

type ctxKey string

const userKey ctxKey = "user"

// userFrom returns the signed-in account for a request.
func userFrom(r *http.Request) store.User {
	if u, ok := r.Context().Value(userKey).(store.User); ok {
		return u
	}
	return store.User{}
}

// adminOnlyPaths are readable only by administrators. Everything else follows
// the general rule below — read-only accounts may GET but never change — but
// account management and the audit trail are not things a read-only operator
// should see at all.
//
// Unlike the write rule, this list does not maintain itself: a new admin-only
// page has to be added here by hand. TestAdminOnlyPagesAreGated exists to catch
// the case where someone forgets, which has already happened once.
var adminOnlyPaths = []string{"/settings", "/api/users", "/audit", "/api/audit"}

// requireAuth gates the whole admin surface on a login, and enforces the
// read-only role.
//
// The role rule is deliberately a single line: read-only accounts may issue
// GET and HEAD, nothing else. Every mutation in this server is a POST, PUT or
// DELETE, so there is no list of protected endpoints to keep in step with the
// routes — a new mutating endpoint is restricted the moment it is added,
// rather than the moment someone remembers to add it to a list.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Before anyone can log in, someone has to be created.
		configured, err := s.st.HasUsers(r.Context())
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "could not check for accounts", err)
			return
		}
		if !configured {
			s.redirectOrJSON(w, r, "/setup", "this server has no accounts yet; open it in a browser to create the first one")
			return
		}

		user, err := s.currentUser(r)
		if err != nil {
			s.redirectOrJSON(w, r, "/login", "not signed in")
			return
		}

		if !user.IsAdmin() {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				writeJSON(w, http.StatusForbidden, proto.Error{
					Error: "this account is read-only"})
				return
			}
			for _, p := range adminOnlyPaths {
				if r.URL.Path == p || strings.HasPrefix(r.URL.Path, p+"/") {
					s.forbidden(w, r, "account management is available to administrators only")
					return
				}
			}
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}

func (s *Server) currentUser(r *http.Request) (store.User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.User{}, errors.New("no session")
	}
	return s.st.UserForSession(r.Context(), c.Value)
}

// redirectOrJSON sends a browser to a page and an API client an explanation.
func (s *Server) redirectOrJSON(w http.ResponseWriter, r *http.Request, path, message string) {
	if wantsHTML(r) {
		http.Redirect(w, r, path, http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusUnauthorized, proto.Error{Error: message})
}

func (s *Server) forbidden(w http.ResponseWriter, r *http.Request, message string) {
	if wantsHTML(r) {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "forbidden.html", s.pageData(r, "", map[string]any{"Message": message}))
		return
	}
	writeJSON(w, http.StatusForbidden, proto.Error{Error: message})
}

// wantsHTML distinguishes a browser navigating from a script calling the API.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// --- first run ---

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	configured, err := s.st.HasUsers(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not check for accounts", err)
		return
	}
	if configured {
		// Setup is strictly a first-run flow. Leaving it open would be a way to
		// mint an administrator on a running instance.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, "setup.html", map[string]any{"Page": "setup", "Version": Version})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	configured, err := s.st.HasUsers(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not check for accounts", err)
		return
	}
	if configured {
		writeJSON(w, http.StatusConflict, proto.Error{Error: "this server is already set up"})
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// The first account is always an admin: an instance whose only account
	// cannot change anything is unusable.
	user, err := s.st.CreateUser(r.Context(), body.Username, body.Password, store.RoleAdmin)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}

	token, err := s.st.CreateSession(r.Context(), user.ID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not start a session", err)
		return
	}
	s.setSessionCookie(w, r, token)

	s.log.Info("first administrator created", "username", user.Username)
	s.audit(r, store.AuditEntry{
		Username: user.Username, Role: user.Role, Action: "account.create",
		Target: user.Username, Detail: "first administrator, created during setup",
		Status: http.StatusOK,
	})
	writeJSON(w, http.StatusOK, user)
}

// --- login ---

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	configured, err := s.st.HasUsers(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not check for accounts", err)
		return
	}
	if !configured {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, err := s.currentUser(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", map[string]any{"Page": "login", "Version": Version})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	user, err := s.st.AuthenticateUser(r.Context(), body.Username, body.Password)
	if err != nil {
		s.log.Warn("failed login", "username", body.Username, "remote", clientIP(r))
		s.audit(r, store.AuditEntry{
			Username: body.Username, Action: "auth.login-failed",
			Detail: "invalid username or password", Status: http.StatusUnauthorized,
		})
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: store.ErrBadCredentials.Error()})
		return
	}

	token, err := s.st.CreateSession(r.Context(), user.ID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not start a session", err)
		return
	}
	s.setSessionCookie(w, r, token)

	s.log.Info("signed in", "username", user.Username, "role", user.Role, "remote", clientIP(r))
	s.audit(r, store.AuditEntry{
		Username: user.Username, Role: user.Role, Action: "auth.login", Status: http.StatusOK,
	})
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if user, err := s.currentUser(r); err == nil {
		s.audit(r, store.AuditEntry{
			Username: user.Username, Role: user.Role, Action: "auth.logout", Status: http.StatusOK,
		})
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.st.DeleteSession(r.Context(), c.Value); err != nil {
			s.log.Error("could not delete session", "error", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(store.SessionLifetime.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Only over TLS, where the browser will honour it. Setting it on plain
		// HTTP would stop the cookie being sent at all.
		Secure: r.TLS != nil,
	})
}

// --- account management ---

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.ListUsers(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "you": userFrom(r).ID})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	user, err := s.st.CreateUser(r.Context(), body.Username, body.Password, body.Role)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}
	auditf(r, "account.create", user.Username, "role "+user.Role)
	s.log.Info("account created", "username", user.Username, "role", user.Role, "by", adminUser(r))
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == userFrom(r).ID {
		writeJSON(w, http.StatusBadRequest, proto.Error{
			Error: "you cannot delete the account you are signed in with"})
		return
	}
	if err := s.st.DeleteUser(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such account"})
			return
		}
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}
	auditf(r, "account.delete", id, "account removed and its sessions ended")
	s.log.Info("account deleted", "id", id, "by", adminUser(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password,omitempty"`
		Role     string `json:"role,omitempty"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	id := r.PathValue("id")
	if body.Role != "" {
		if id == userFrom(r).ID && body.Role != store.RoleAdmin {
			writeJSON(w, http.StatusBadRequest, proto.Error{
				Error: "you cannot remove your own administrator role"})
			return
		}
		if err := s.st.SetRole(r.Context(), id, body.Role); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such account"})
				return
			}
			writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
			return
		}
		auditf(r, "account.role", id, "role changed to "+body.Role)
		s.log.Info("account role changed", "id", id, "role", body.Role, "by", adminUser(r))
	}

	if body.Password != "" {
		if err := s.st.SetPassword(r.Context(), id, body.Password); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such account"})
				return
			}
			writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
			return
		}
		auditf(r, "account.password", id, "password changed and other sessions ended")
		s.log.Info("account password changed", "id", id, "by", adminUser(r))
	}

	user, err := s.st.GetUser(r.Context(), id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the account", err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	theme, _ := s.st.Theme(r.Context())
	tz, _ := s.st.Timezone(r.Context())
	s.render(w, "settings.html", s.pageData(r, "settings", map[string]any{
		"Themes":   store.Themes,
		"Theme":    theme,
		"Timezone": tz,
		"Zones":    commonZones,
	}))
}

// pageData assembles what every page needs: who is signed in, and the display
// settings the layout and scripts depend on. Centralised so a new page cannot
// accidentally render without a theme or with the wrong time zone.
func (s *Server) pageData(r *http.Request, page string, extra map[string]any) map[string]any {
	theme, err := s.st.Theme(r.Context())
	if err != nil {
		theme = store.DefaultTheme
	}
	tz, err := s.st.Timezone(r.Context())
	if err != nil {
		tz = "UTC"
	}

	data := map[string]any{
		"Page":     page,
		"Version":  Version,
		"User":     userFrom(r),
		"Theme":    theme,
		"Timezone": tz,
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

// audit records an entry for a request outside the change-recording middleware
// — the sign-in and setup routes, which are reachable without a session.
func (s *Server) audit(r *http.Request, e store.AuditEntry) {
	e.Method = r.Method
	e.Path = r.URL.Path
	e.IP = clientIP(r)
	if err := s.st.AppendAudit(r.Context(), e); err != nil {
		s.log.Error("could not write audit entry", "action", e.Action, "error", err)
	}
}
