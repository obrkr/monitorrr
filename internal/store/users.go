package store

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Roles.
//
// Deliberately only two. Anything finer needs a permission model, and a home
// lab does not have one — "can change things" and "can look" covers it.
const (
	RoleAdmin    = "admin"
	RoleReadOnly = "readonly"
)

// SessionLifetime is how long a login lasts without activity.
const SessionLifetime = 7 * 24 * time.Hour

// pbkdf2Iterations follows the OWASP recommendation for PBKDF2-HMAC-SHA256.
// A variable rather than a constant so tests can turn it down: at this cost
// each hash is deliberately about a third of a second, which is the point for
// a login and unbearable across a test suite.
var pbkdf2Iterations = 600_000

// ErrBadCredentials is returned for any failed login, whatever the reason.
// The caller must not distinguish "no such user" from "wrong password", or the
// error itself becomes a way to enumerate accounts.
var ErrBadCredentials = errors.New("invalid username or password")

// User is an operator account.
type User struct {
	ID        string     `json:"id"`
	Username  string     `json:"username"`
	Role      string     `json:"role"`
	CreatedAt time.Time  `json:"created_at"`
	LastLogin *time.Time `json:"last_login,omitempty"`
}

// IsAdmin reports whether this account may change anything.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// ValidRole reports whether a role name is one we support.
func ValidRole(role string) bool { return role == RoleAdmin || role == RoleReadOnly }

// HasUsers reports whether any account exists. False means the server has never
// been set up and the first-run flow should run.
func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return n > 0, nil
}

// CreateUser adds an account. The first account must be an admin, or the
// instance would be left with nobody able to administer it.
func (s *Store) CreateUser(ctx context.Context, username, password, role string) (User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return User{}, errors.New("a username is required")
	}
	if len(username) > 64 {
		return User{}, errors.New("username must be 64 characters or fewer")
	}
	if len(password) < 8 {
		return User{}, errors.New("password must be at least 8 characters")
	}
	if !ValidRole(role) {
		return User{}, fmt.Errorf("unknown role %q", role)
	}

	hash, salt, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	id, err := randomToken()
	if err != nil {
		return User{}, err
	}
	now := time.Now()

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, salt, iterations, role, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, username, hash, salt, pbkdf2Iterations, role, now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, fmt.Errorf("an account named %q already exists", username)
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return User{ID: id, Username: username, Role: role, CreatedAt: now}, nil
}

// AuthenticateUser verifies a username and password.
func (s *Store) AuthenticateUser(ctx context.Context, username, password string) (User, error) {
	username = strings.ToLower(strings.TrimSpace(username))

	var (
		u          User
		storedHash string
		salt       string
		iterations int
		created    int64
		lastLogin  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, salt, iterations, role, created_at, last_login
		 FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &storedHash, &salt, &iterations, &u.Role, &created, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		// Spend the time anyway. Returning immediately for an unknown user
		// makes login timing a way to discover which accounts exist.
		_, _, _ = hashPassword(password)
		return User{}, ErrBadCredentials
	}
	if err != nil {
		return User{}, fmt.Errorf("read user: %w", err)
	}

	saltBytes, err := hex.DecodeString(salt)
	if err != nil {
		return User{}, fmt.Errorf("decode salt: %w", err)
	}
	computed, err := pbkdf2.Key(sha256.New, password, saltBytes, iterations, 32)
	if err != nil {
		return User{}, fmt.Errorf("derive key: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(computed)), []byte(storedHash)) != 1 {
		return User{}, ErrBadCredentials
	}

	u.CreatedAt = time.Unix(created, 0)
	if lastLogin.Valid {
		t := time.Unix(lastLogin.Int64, 0)
		u.LastLogin = &t
	}
	return u, nil
}

// ListUsers returns every account, oldest first.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, role, created_at, last_login FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	users := []User{}
	for rows.Next() {
		var (
			u         User
			created   int64
			lastLogin sql.NullInt64
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &created, &lastLogin); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.CreatedAt = time.Unix(created, 0)
		if lastLogin.Valid {
			t := time.Unix(lastLogin.Int64, 0)
			u.LastLogin = &t
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// GetUser returns one account.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	var (
		u         User
		created   int64
		lastLogin sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, role, created_at, last_login FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Username, &u.Role, &created, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("read user: %w", err)
	}
	u.CreatedAt = time.Unix(created, 0)
	if lastLogin.Valid {
		t := time.Unix(lastLogin.Int64, 0)
		u.LastLogin = &t
	}
	return u, nil
}

// DeleteUser removes an account. The last admin cannot be removed: an instance
// with no administrator cannot be recovered through the UI at all.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete user: %w", err)
	}
	defer tx.Rollback()

	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, id).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read user: %w", err)
	}

	if role == RoleAdmin {
		var admins int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&admins); err != nil {
			return fmt.Errorf("count admins: %w", err)
		}
		if admins <= 1 {
			return errors.New("this is the only admin account; create another before removing it")
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	// Sessions go with the account, so removing access takes effect at once
	// rather than whenever the cookie happens to expire.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	return tx.Commit()
}

// SetPassword changes an account's password and ends its other sessions.
func (s *Store) SetPassword(ctx context.Context, id, password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, salt, err := hashPassword(password)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, salt = ?, iterations = ? WHERE id = ?`,
		hash, salt, pbkdf2Iterations, id)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	// Whoever knew the old password should not keep a live session.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("clear sessions: %w", err)
	}
	return nil
}

// SetRole changes an account's role, refusing to remove the last admin.
func (s *Store) SetRole(ctx context.Context, id, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set role: %w", err)
	}
	defer tx.Rollback()

	var current string
	err = tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read user: %w", err)
	}

	if current == RoleAdmin && role != RoleAdmin {
		var admins int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&admins); err != nil {
			return fmt.Errorf("count admins: %w", err)
		}
		if admins <= 1 {
			return errors.New("this is the only admin account; promote another before demoting it")
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id); err != nil {
		return fmt.Errorf("set role: %w", err)
	}
	return tx.Commit()
}

// --- sessions ---

// CreateSession issues a login token. Only its hash is stored, so a copy of the
// database does not hand over live sessions.
func (s *Store) CreateSession(ctx context.Context, userID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := hex.EncodeToString(raw)
	now := time.Now()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		hashToken(token), userID, now.Unix(), now.Add(SessionLifetime).Unix(),
	); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_login = ? WHERE id = ?`, now.Unix(), userID); err != nil {
		return "", fmt.Errorf("record login: %w", err)
	}
	return token, nil
}

// UserForSession resolves a session token, extending it on use so an active
// operator is not logged out mid-session.
func (s *Store) UserForSession(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNotFound
	}
	hash := hashToken(token)

	var (
		userID  string
		expires int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`, hash).Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("read session: %w", err)
	}
	if time.Now().Unix() > expires {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash)
		return User{}, ErrNotFound
	}

	user, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, err
	}

	// Slide the expiry, but only when it is worth a write.
	if time.Until(time.Unix(expires, 0)) < SessionLifetime-time.Hour {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE sessions SET expires_at = ? WHERE token_hash = ?`,
			time.Now().Add(SessionLifetime).Unix(), hash)
	}
	return user, nil
}

// DeleteSession logs out one session.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// SweepExpiredSessions removes logins that have lapsed.
func (s *Store) SweepExpiredSessions(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("sweep sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// hashPassword derives a verifier and returns it with its salt, both hex.
func hashPassword(password string) (hash, salt string, err error) {
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", "", fmt.Errorf("generate salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, saltBytes, pbkdf2Iterations, 32)
	if err != nil {
		return "", "", fmt.Errorf("derive key: %w", err)
	}
	return hex.EncodeToString(key), hex.EncodeToString(saltBytes), nil
}

// ResetSummary counts what a reset removed.
type ResetSummary struct {
	Devices     int `json:"devices"`
	Scripts     int `json:"scripts"`
	Jobs        int `json:"jobs"`
	Collections int `json:"collections"`
	Payloads    int `json:"payloads"`
	Events      int `json:"events"`
}

// ResetFleet empties everything about the fleet, keeping accounts.
//
// Accounts survive deliberately: wiping them would lock the operator out of the
// server they just reset, with no way back in short of deleting the database.
// It runs as one transaction, so a reset either happens or does not — a
// half-cleared instance would be worse than either.
func (s *Store) ResetFleet(ctx context.Context) (ResetSummary, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ResetSummary{}, fmt.Errorf("begin reset: %w", err)
	}
	defer tx.Rollback()

	var summary ResetSummary
	counts := []struct {
		table string
		into  *int
	}{
		{"devices", &summary.Devices},
		{"scripts", &summary.Scripts},
		{"jobs", &summary.Jobs},
		{"collections", &summary.Collections},
		{"payloads", &summary.Payloads},
		{"device_events", &summary.Events},
	}
	for _, c := range counts {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+c.table).Scan(c.into); err != nil {
			return ResetSummary{}, fmt.Errorf("count %s: %w", c.table, err)
		}
	}

	// Devices cascade to their events, jobs and collections, but the others are
	// cleared explicitly rather than relying on that: a table that stops
	// cascading later should not quietly start surviving resets.
	for _, table := range []string{"jobs", "collections", "device_events", "devices", "scripts", "payloads"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return ResetSummary{}, fmt.Errorf("clear %s: %w", table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return ResetSummary{}, fmt.Errorf("commit reset: %w", err)
	}
	return summary, nil
}
