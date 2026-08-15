package server

import (
	"net/http"
	"testing"

	"github.com/ollie/monitorrr/internal/store"
)

// The audit log has to be comprehensive by construction, or it is worse than
// useless: a log with gaps invites the belief that anything missing did not
// happen.
func TestEveryChangeIsRecorded(t *testing.T) {
	srv, st := testServer(t)
	admin := signIn(t, srv, st, "ollie", store.RoleAdmin)

	changes := []struct{ method, path string }{
		{http.MethodPost, "/api/settings/interval"},
		{http.MethodPost, "/api/settings/rotate-token"},
		{http.MethodPost, "/api/settings/auto-update"},
		{http.MethodPost, "/api/settings/theme"},
		{http.MethodPost, "/api/users"},
	}
	for _, c := range changes {
		do(srv, c.method, c.path, admin)
	}

	entries, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit entries recorded for a series of changes")
	}

	// Only the successful ones. An empty body makes several of those requests
	// invalid, and recording a rejected request as an action would make the log
	// describe things that never happened.
	for _, e := range entries {
		if e.Status >= 400 {
			t.Errorf("a failed request was recorded as an action: %s %s → %d",
				e.Method, e.Path, e.Status)
		}
		if e.Username != "ollie" {
			t.Errorf("entry attributed to %q, want ollie", e.Username)
		}
		if e.Action == "" {
			t.Errorf("entry for %s has no action name", e.Path)
		}
	}
}

// Reads must not be recorded, or the log becomes noise nobody reads.
func TestReadsAreNotRecorded(t *testing.T) {
	srv, st := testServer(t)
	admin := signIn(t, srv, st, "ollie", store.RoleAdmin)

	before, _ := st.AuditCount(t.Context())
	for _, path := range []string{"/", "/api/devices", "/api/scripts", "/api/jobs", "/audit"} {
		do(srv, http.MethodGet, path, admin)
	}
	after, _ := st.AuditCount(t.Context())

	if after != before {
		t.Errorf("%d entries recorded for read-only requests, want 0", after-before)
	}
}

// A read-only account cannot change anything, so it must generate no entries
// either — and the refusals must not be recorded as actions.
func TestRefusedChangesAreNotRecordedAsActions(t *testing.T) {
	srv, st := testServer(t)
	signIn(t, srv, st, "ollie", store.RoleAdmin)
	viewer := signIn(t, srv, st, "viewer", store.RoleReadOnly)

	before, _ := st.AuditCount(t.Context())
	do(srv, http.MethodPost, "/api/settings/interval", viewer)
	do(srv, http.MethodDelete, "/api/devices/anything", viewer)
	after, _ := st.AuditCount(t.Context())

	if after != before {
		t.Errorf("%d entries recorded for refused requests, want 0", after-before)
	}
}

// Sign-ins sit outside the change-recording middleware, and a log that shows
// only successful access misses the part worth reviewing.
func TestLoginsAreRecordedIncludingFailures(t *testing.T) {
	srv, st := testServer(t)
	if _, err := st.CreateUser(t.Context(), "ollie", "correcthorse", store.RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}

	postJSON(srv, "/login", `{"username":"ollie","password":"correcthorse"}`)
	postJSON(srv, "/login", `{"username":"ollie","password":"wrong"}`)
	postJSON(srv, "/login", `{"username":"ghost","password":"whatever"}`)

	entries, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}

	var ok, failed int
	for _, e := range entries {
		switch e.Action {
		case "auth.login":
			ok++
		case "auth.login-failed":
			failed++
		}
	}
	if ok != 1 {
		t.Errorf("recorded %d successful sign-ins, want 1", ok)
	}
	if failed != 2 {
		t.Errorf("recorded %d failed sign-ins, want 2", failed)
	}
}

func TestAuditFilters(t *testing.T) {
	ctx := t.Context()
	srv, st := testServer(t)
	admin := signIn(t, srv, st, "ollie", store.RoleAdmin)
	do(srv, http.MethodPost, "/api/settings/rotate-token", admin)
	do(srv, http.MethodPost, "/api/settings/auto-update", admin)

	all, _ := st.ListAudit(ctx, store.AuditFilter{})
	if len(all) < 2 {
		t.Fatalf("expected at least two entries, got %d", len(all))
	}

	byAction, err := st.ListAudit(ctx, store.AuditFilter{Action: all[0].Action})
	if err != nil {
		t.Fatalf("filter by action: %v", err)
	}
	for _, e := range byAction {
		if e.Action != all[0].Action {
			t.Errorf("action filter returned %q, want %q", e.Action, all[0].Action)
		}
	}

	if none, _ := st.ListAudit(ctx, store.AuditFilter{Username: "nobody"}); len(none) != 0 {
		t.Errorf("filtering by an unknown user returned %d entries", len(none))
	}

	// Newest first, so paging back through history is well defined.
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatalf("entries are not in descending id order at %d", i)
		}
	}
}

func TestDescribeRouteNamesActionsSensibly(t *testing.T) {
	cases := []struct{ method, path, wantAction, wantTarget string }{
		{http.MethodPost, "/api/scripts", "script.create", ""},
		{http.MethodDelete, "/api/scripts/abc", "script.delete", "abc"},
		{http.MethodDelete, "/api/devices/xyz", "device.delete", "xyz"},
		{http.MethodPost, "/api/devices/xyz/retire", "device.retire", "xyz"},
		{http.MethodPost, "/api/devices/xyz/collect", "device.collect", "xyz"},
		{http.MethodPatch, "/api/users/u1", "user.update", "u1"},
		{http.MethodPost, "/api/admin/reset", "admin.reset", ""},
		{http.MethodPost, "/api/settings/interval", "settings.interval", ""},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(c.method, c.path, nil)
		action, target := describeRoute(r)
		if action != c.wantAction || target != c.wantTarget {
			t.Errorf("%s %s → (%q, %q), want (%q, %q)",
				c.method, c.path, action, target, c.wantAction, c.wantTarget)
		}
	}
}

// Display settings are instance-wide, and an unusable value must never be
// stored — a bad zone would break every timestamp in the console.
func TestDisplaySettings(t *testing.T) {
	ctx := t.Context()
	_, st := testServer(t)

	if tz, _ := st.Timezone(ctx); tz != "UTC" {
		t.Errorf("default timezone = %q, want UTC", tz)
	}
	if theme, _ := st.Theme(ctx); theme != store.DefaultTheme {
		t.Errorf("default theme = %q, want %q", theme, store.DefaultTheme)
	}

	if err := st.SetTimezone(ctx, "Europe/London"); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	if tz, _ := st.Timezone(ctx); tz != "Europe/London" {
		t.Errorf("timezone = %q, want Europe/London", tz)
	}
	if err := st.SetTimezone(ctx, "Mars/Olympus_Mons"); err == nil {
		t.Error("an invalid time zone was accepted")
	}
	// The bad value must not have replaced the good one.
	if tz, _ := st.Timezone(ctx); tz != "Europe/London" {
		t.Errorf("timezone = %q after a rejected change, want it unchanged", tz)
	}

	for _, theme := range store.Themes {
		if err := st.SetTheme(ctx, theme); err != nil {
			t.Errorf("theme %q rejected: %v", theme, err)
		}
	}
	if err := st.SetTheme(ctx, "hot-pink"); err == nil {
		t.Error("an unknown theme was accepted")
	}
}

// A factory reset must leave nothing behind, and must genuinely return the
// server to first-run rather than to a half-configured state that neither
// serves the console nor offers setup.
func TestFactoryResetReturnsToFirstRun(t *testing.T) {
	ctx := t.Context()
	srv, st := testServer(t)
	admin := signIn(t, srv, st, "ollie", store.RoleAdmin)
	signIn(t, srv, st, "viewer", store.RoleReadOnly)

	// Give it something to lose.
	do(srv, http.MethodPost, "/api/scripts/starter", admin)
	do(srv, http.MethodPost, "/api/settings/rotate-token", admin)
	if _, err := st.CreateUser(ctx, "third", "correcthorse", store.RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}

	oldToken, _ := st.EnrollToken(ctx)

	if rec := do(srv, http.MethodPost, "/api/admin/factory-reset", admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("factory reset without confirmation = %d, want 400", rec.Code)
	}
	// The wrong word — the one that confirms the lesser reset — must not do it.
	if rec := postAs(srv, admin, "/api/admin/factory-reset", `{"confirm":"RESET"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("factory reset confirmed with RESET = %d, want 400", rec.Code)
	}

	rec := postAs(srv, admin, "/api/admin/factory-reset", `{"confirm":"FACTORY RESET"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("factory reset = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if has, _ := st.HasUsers(ctx); has {
		t.Error("accounts survived a factory reset")
	}
	if n, _ := st.AuditCount(ctx); n != 0 {
		t.Errorf("%d audit entries survived a factory reset", n)
	}
	if scripts, _ := st.ListScripts(ctx); len(scripts) != 0 {
		t.Errorf("%d scripts survived a factory reset", len(scripts))
	}

	// Settings are cleared too, but the instance must still be usable: an
	// enrollment token and interval are reissued rather than left empty.
	newToken, err := st.EnrollToken(ctx)
	if err != nil || newToken == "" {
		t.Fatalf("enrollment token after reset = (%q, %v), want a fresh one", newToken, err)
	}
	if newToken == oldToken {
		t.Error("the enrollment token survived a factory reset")
	}
	if interval, _ := st.DefaultCheckinInterval(ctx); interval != store.DefaultInterval {
		t.Errorf("check-in interval = %d after reset, want the default %d", interval, store.DefaultInterval)
	}

	// And the console is back to first run for everyone, including whoever had
	// just been signed in as an administrator.
	if rec := do(srv, http.MethodGet, "/api/devices", admin); rec.Code != http.StatusUnauthorized {
		t.Errorf("API with the old session after reset = %d, want 401", rec.Code)
	}
	if rec := do(srv, http.MethodGet, "/setup", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /setup after a factory reset = %d, want the setup page", rec.Code)
	}
}
