package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollie/monitorrr/internal/store"
)

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := New(Config{DBPath: filepath.Join(dir, "test.db"), DistDir: dir},
		st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st
}

// signIn creates an account and returns a request cookie for it.
func signIn(t *testing.T, srv *Server, st *store.Store, username, role string) *http.Cookie {
	t.Helper()
	user, err := st.CreateUser(t.Context(), username, "correcthorse", role)
	if err != nil {
		t.Fatalf("create %s: %v", username, err)
	}
	token, err := st.CreateSession(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return &http.Cookie{Name: sessionCookie, Value: token}
}

func do(srv *Server, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// Until an account exists the server must not serve its data to anyone.
func TestUnconfiguredServerExposesNothing(t *testing.T) {
	srv, _ := testServer(t)

	for _, path := range []string{"/", "/api/devices", "/api/scripts", "/settings"} {
		rec := do(srv, http.MethodGet, path, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s returned 200 before any account existed", path)
		}
	}
}

func TestSignedOutIsRejected(t *testing.T) {
	srv, st := testServer(t)
	signIn(t, srv, st, "ollie", store.RoleAdmin) // an account now exists

	for _, path := range []string{"/api/devices", "/api/scripts", "/api/users"} {
		rec := do(srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, rec.Code)
		}
	}

	// A bogus cookie is no better than none.
	bad := &http.Cookie{Name: sessionCookie, Value: "not-a-session"}
	if rec := do(srv, http.MethodGet, "/api/devices", bad); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET with an invalid session = %d, want 401", rec.Code)
	}
}

// The whole point of the read-only role: it can look at everything and change
// nothing.
func TestReadOnlyCanReadButNotWrite(t *testing.T) {
	srv, st := testServer(t)
	signIn(t, srv, st, "ollie", store.RoleAdmin)
	viewer := signIn(t, srv, st, "viewer", store.RoleReadOnly)

	t.Run("reads are allowed", func(t *testing.T) {
		for _, path := range []string{"/", "/api/devices", "/api/scripts", "/api/jobs", "/api/tags", "/runs"} {
			if rec := do(srv, http.MethodGet, path, viewer); rec.Code != http.StatusOK {
				t.Errorf("GET %s as read-only = %d, want 200", path, rec.Code)
			}
		}
	})

	t.Run("every mutation is refused", func(t *testing.T) {
		mutations := []struct{ method, path string }{
			{http.MethodPost, "/api/scripts"},
			{http.MethodPost, "/api/scripts/starter"},
			{http.MethodDelete, "/api/scripts/anything"},
			{http.MethodPost, "/api/dispatch"},
			{http.MethodPost, "/api/devices/anything/dispatch"},
			{http.MethodPost, "/api/devices/anything/retire"},
			{http.MethodPost, "/api/devices/anything/collect"},
			{http.MethodPost, "/api/devices/anything/tags"},
			{http.MethodDelete, "/api/devices/anything"},
			{http.MethodPost, "/api/settings/interval"},
			{http.MethodPost, "/api/settings/rotate-token"},
			{http.MethodPost, "/api/settings/auto-update"},
			{http.MethodPost, "/api/payloads"},
			{http.MethodPost, "/api/users"},
			{http.MethodPost, "/api/admin/retire-all"},
			{http.MethodPost, "/api/admin/reset"},
		}
		for _, m := range mutations {
			rec := do(srv, m.method, m.path, viewer)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as read-only = %d, want 403", m.method, m.path, rec.Code)
			}
		}
	})

	t.Run("admin-only pages are hidden entirely", func(t *testing.T) {
		// Not merely unwritable: a read-only operator should not be able to
		// enumerate accounts or read the audit trail either.
		for _, path := range []string{"/settings", "/api/users", "/audit", "/api/audit"} {
			if rec := do(srv, http.MethodGet, path, viewer); rec.Code != http.StatusForbidden {
				t.Errorf("GET %s as read-only = %d, want 403", path, rec.Code)
			}
		}
	})
}

func TestAdminCanReachEverything(t *testing.T) {
	srv, st := testServer(t)
	admin := signIn(t, srv, st, "ollie", store.RoleAdmin)

	for _, path := range []string{"/", "/settings", "/api/users", "/api/devices", "/deployment"} {
		if rec := do(srv, http.MethodGet, path, admin); rec.Code != http.StatusOK {
			t.Errorf("GET %s as admin = %d, want 200", path, rec.Code)
		}
	}
	// A mutation an admin is entitled to must not be refused by the role check.
	// (400 is fine here — the empty body is invalid — 403 is not.)
	if rec := do(srv, http.MethodPost, "/api/settings/interval", admin); rec.Code == http.StatusForbidden {
		t.Error("an admin was refused a settings change")
	}
}

// Hiding a page in the navigation is presentation, not access control. This
// asserts that every page an admin-only link points at is actually gated in the
// middleware — the mistake this catches was real: /audit was hidden from the
// menu but served to anyone who typed the URL.
func TestAdminOnlyPagesAreGated(t *testing.T) {
	srv, st := testServer(t)
	signIn(t, srv, st, "ollie", store.RoleAdmin)
	viewer := signIn(t, srv, st, "viewer", store.RoleReadOnly)

	// Every path the layout renders only for admins, plus its API.
	adminPages := []string{"/settings", "/audit", "/api/users", "/api/audit"}
	for _, path := range adminPages {
		rec := do(srv, http.MethodGet, path, viewer)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as read-only = %d, want 403 — hidden in the menu is not the same as gated",
				path, rec.Code)
		}
	}
}

// Setup is a first-run flow only. If it stayed open it would be a way to mint
// an administrator on a running instance without signing in.
func TestSetupClosesAfterFirstAccount(t *testing.T) {
	srv, st := testServer(t)

	if rec := do(srv, http.MethodGet, "/setup", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /setup on a fresh server = %d, want 200", rec.Code)
	}

	signIn(t, srv, st, "ollie", store.RoleAdmin)

	if rec := do(srv, http.MethodPost, "/setup", nil); rec.Code != http.StatusConflict {
		t.Errorf("POST /setup after setup = %d, want 409", rec.Code)
	}
	if rec := do(srv, http.MethodGet, "/setup", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("GET /setup after setup = %d, want a redirect to login", rec.Code)
	}
}

// Agents authenticate with device tokens and must be unaffected by operator
// accounts existing at all — otherwise adding logins would silently break the
// entire fleet.
func TestAgentEndpointsDoNotRequireLogin(t *testing.T) {
	srv, st := testServer(t)
	signIn(t, srv, st, "ollie", store.RoleAdmin)

	if rec := do(srv, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}

	// No session, and it must fail on device credentials rather than be
	// redirected to a login page.
	rec := do(srv, http.MethodPost, "/v1/checkin", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /v1/checkin without credentials = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "not signed in") {
		t.Error("an agent endpoint answered with an operator login error")
	}
}

// postJSON issues an unauthenticated JSON POST, for the sign-in routes.
func postJSON(srv *Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// postAs issues a JSON POST with a body, as a signed-in account.
func postAs(srv *Server, cookie *http.Cookie, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}
