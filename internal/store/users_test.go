package store

import (
	"context"
	"testing"
	"time"
)

// Password hashing is deliberately expensive, which makes the real cost
// unbearable across a suite. Correctness does not depend on the iteration
// count, so tests turn it down.
func init() { pbkdf2Iterations = 1000 }

func TestFirstRunDetection(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	has, err := st.HasUsers(ctx)
	if err != nil || has {
		t.Fatalf("HasUsers on a fresh database = (%v, %v), want (false, nil)", has, err)
	}

	if _, err := st.CreateUser(ctx, "ollie", "correcthorse", RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}

	if has, _ := st.HasUsers(ctx); !has {
		t.Error("HasUsers = false after an account was created")
	}
}

func TestCreateUserValidation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	cases := []struct {
		name, user, pass, role string
	}{
		{"no username", "", "correcthorse", RoleAdmin},
		{"whitespace username", "   ", "correcthorse", RoleAdmin},
		{"short password", "ollie", "short", RoleAdmin},
		{"unknown role", "ollie", "correcthorse", "superuser"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.CreateUser(ctx, tc.user, tc.pass, tc.role); err == nil {
				t.Error("want an error, got nil")
			}
		})
	}

	if _, err := st.CreateUser(ctx, "Ollie", "correcthorse", RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Usernames are matched case-insensitively, so "OLLIE" must not become a
	// second account that shadows the first at login.
	if _, err := st.CreateUser(ctx, "OLLIE", "correcthorse", RoleAdmin); err == nil {
		t.Error("duplicate username differing only by case was accepted")
	}
}

func TestAuthentication(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	created, err := st.CreateUser(ctx, "ollie", "correcthorse", RoleAdmin)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	user, err := st.AuthenticateUser(ctx, "ollie", "correcthorse")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if user.ID != created.ID || !user.IsAdmin() {
		t.Errorf("authenticated as %+v, want the created admin", user)
	}

	// Username matching is case-insensitive; passwords are not.
	if _, err := st.AuthenticateUser(ctx, "OLLIE", "correcthorse"); err != nil {
		t.Errorf("case-insensitive username login failed: %v", err)
	}
	if _, err := st.AuthenticateUser(ctx, "ollie", "CorrectHorse"); err != ErrBadCredentials {
		t.Errorf("wrong-case password = %v, want ErrBadCredentials", err)
	}
	if _, err := st.AuthenticateUser(ctx, "ollie", "wrong"); err != ErrBadCredentials {
		t.Errorf("wrong password = %v, want ErrBadCredentials", err)
	}
	// An unknown user must be indistinguishable from a wrong password, or the
	// error itself enumerates accounts.
	if _, err := st.AuthenticateUser(ctx, "nobody", "correcthorse"); err != ErrBadCredentials {
		t.Errorf("unknown user = %v, want the same error as a wrong password", err)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	user, _ := st.CreateUser(ctx, "ollie", "correcthorse", RoleAdmin)
	token, err := st.CreateSession(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	got, err := st.UserForSession(ctx, token)
	if err != nil || got.ID != user.ID {
		t.Fatalf("UserForSession = (%+v, %v), want the signed-in user", got, err)
	}

	if _, err := st.UserForSession(ctx, "not-a-real-token"); err != ErrNotFound {
		t.Errorf("bogus token = %v, want ErrNotFound", err)
	}
	if _, err := st.UserForSession(ctx, ""); err != ErrNotFound {
		t.Errorf("empty token = %v, want ErrNotFound", err)
	}

	// Signing in records when.
	after, _ := st.GetUser(ctx, user.ID)
	if after.LastLogin == nil {
		t.Error("last_login not recorded on sign-in")
	}

	if err := st.DeleteSession(ctx, token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := st.UserForSession(ctx, token); err != ErrNotFound {
		t.Error("session still valid after sign-out")
	}
}

func TestExpiredSessionsAreRejectedAndSwept(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	user, _ := st.CreateUser(ctx, "ollie", "correcthorse", RoleAdmin)
	token, _ := st.CreateSession(ctx, user.ID)

	if _, err := st.db.ExecContext(ctx,
		`UPDATE sessions SET expires_at = ?`, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := st.UserForSession(ctx, token); err != ErrNotFound {
		t.Error("an expired session was accepted")
	}

	// Rejecting it also removes it, so the sweep finds nothing left.
	n, err := st.SweepExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d sessions, want 0 — the rejected one should already be gone", n)
	}
}

// Locking every administrator out of an instance is unrecoverable through the
// UI, so the last one is protected from both deletion and demotion.
func TestLastAdminIsProtected(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	admin, _ := st.CreateUser(ctx, "ollie", "correcthorse", RoleAdmin)
	viewer, _ := st.CreateUser(ctx, "viewer", "correcthorse", RoleReadOnly)

	if err := st.DeleteUser(ctx, admin.ID); err == nil {
		t.Error("deleting the only admin was allowed")
	}
	if err := st.SetRole(ctx, admin.ID, RoleReadOnly); err == nil {
		t.Error("demoting the only admin was allowed")
	}

	// A read-only account is not a protected one.
	if err := st.DeleteUser(ctx, viewer.ID); err != nil {
		t.Errorf("deleting a read-only account: %v", err)
	}

	// With a second admin, the first is no longer the last.
	second, _ := st.CreateUser(ctx, "second", "correcthorse", RoleAdmin)
	if err := st.SetRole(ctx, admin.ID, RoleReadOnly); err != nil {
		t.Errorf("demoting an admin while another exists: %v", err)
	}
	// And now the second one is the protected one.
	if err := st.DeleteUser(ctx, second.ID); err == nil {
		t.Error("deleting the now-only admin was allowed")
	}
}

// Changing a password must end the sessions opened with the old one.
func TestPasswordChangeEndsSessions(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	user, _ := st.CreateUser(ctx, "ollie", "correcthorse", RoleAdmin)
	token, _ := st.CreateSession(ctx, user.ID)

	if err := st.SetPassword(ctx, user.ID, "a-brand-new-password"); err != nil {
		t.Fatalf("set password: %v", err)
	}

	if _, err := st.UserForSession(ctx, token); err != ErrNotFound {
		t.Error("a session opened with the old password survived the change")
	}
	if _, err := st.AuthenticateUser(ctx, "ollie", "correcthorse"); err != ErrBadCredentials {
		t.Error("the old password still works")
	}
	if _, err := st.AuthenticateUser(ctx, "ollie", "a-brand-new-password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if err := st.SetPassword(ctx, user.ID, "short"); err == nil {
		t.Error("a too-short password was accepted")
	}
}

// Deleting an account must revoke its access immediately, not whenever its
// cookie happens to lapse.
func TestDeletingAnAccountEndsItsSessions(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := st.CreateUser(ctx, "ollie", "correcthorse", RoleAdmin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	viewer, _ := st.CreateUser(ctx, "viewer", "correcthorse", RoleReadOnly)
	token, _ := st.CreateSession(ctx, viewer.ID)

	if err := st.DeleteUser(ctx, viewer.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.UserForSession(ctx, token); err != ErrNotFound {
		t.Error("a deleted account's session still works")
	}
}
