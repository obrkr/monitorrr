package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AuditEntry is one recorded change.
type AuditEntry struct {
	ID       int64     `json:"id"`
	TS       time.Time `json:"ts"`
	Username string    `json:"username"`
	Role     string    `json:"role"`
	Action   string    `json:"action"`
	// Not omitempty: a log viewer renders these as columns, and a key that
	// vanishes when the value is blank forces every client to special-case it.
	Target string `json:"target"`
	Detail string `json:"detail"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	IP     string `json:"ip"`
}

// AppendAudit records an action. Failures are returned rather than swallowed,
// but callers generally log and continue: losing an audit line is bad, failing
// the operation the operator actually asked for is worse.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, username, role, action, target, detail, method, path, status, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS.Unix(), e.Username, e.Role, e.Action, e.Target, e.Detail,
		e.Method, e.Path, e.Status, e.IP)
	if err != nil {
		return fmt.Errorf("append audit: %w", err)
	}
	return nil
}

// AuditFilter narrows a query of the log.
type AuditFilter struct {
	Username string
	Action   string
	Search   string
	Limit    int
	Before   int64 // id, for paging back through history
}

// ListAudit returns entries newest first.
func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}

	query := `SELECT id, ts, username, role, action, target, detail, method, path, status, ip
	          FROM audit_log WHERE 1=1`
	args := []any{}

	if f.Username != "" {
		query += ` AND username = ?`
		args = append(args, f.Username)
	}
	if f.Action != "" {
		query += ` AND action = ?`
		args = append(args, f.Action)
	}
	if f.Search != "" {
		query += ` AND (target LIKE ? OR detail LIKE ? OR path LIKE ?)`
		like := "%" + f.Search + "%"
		args = append(args, like, like, like)
	}
	if f.Before > 0 {
		query += ` AND id < ?`
		args = append(args, f.Before)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	entries := []AuditEntry{}
	for rows.Next() {
		var (
			e  AuditEntry
			ts int64
		)
		if err := rows.Scan(&e.ID, &ts, &e.Username, &e.Role, &e.Action, &e.Target,
			&e.Detail, &e.Method, &e.Path, &e.Status, &e.IP); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		e.TS = time.Unix(ts, 0)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// AuditActions lists the distinct action names present, for the filter menu.
func (s *Store) AuditActions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT action FROM audit_log ORDER BY action`)
	if err != nil {
		return nil, fmt.Errorf("list audit actions: %w", err)
	}
	defer rows.Close()

	actions := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

// AuditCount reports how many entries exist.
func (s *Store) AuditCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count audit: %w", err)
	}
	return n, nil
}

// --- display settings ---

const (
	settingTheme    = "theme"
	settingTimezone = "timezone"
)

// DefaultTheme is what a fresh instance uses.
const DefaultTheme = "dark"

// Themes are the selectable colour schemes. Kept here rather than in the
// browser so the server can reject anything it does not know how to render.
var Themes = []string{
	"system", "dark", "light", "nord", "dracula", "solarized-dark", "gruvbox",
}

// ValidTheme reports whether a theme name is one we ship.
func ValidTheme(name string) bool {
	for _, t := range Themes {
		if t == name {
			return true
		}
	}
	return false
}

// Theme returns the configured colour scheme.
func (s *Store) Theme(ctx context.Context) (string, error) {
	v, err := s.setting(ctx, settingTheme)
	if err != nil {
		return DefaultTheme, nil
	}
	if !ValidTheme(v) {
		return DefaultTheme, nil
	}
	return v, nil
}

// SetTheme changes the colour scheme.
func (s *Store) SetTheme(ctx context.Context, name string) error {
	if !ValidTheme(name) {
		return fmt.Errorf("unknown theme %q", name)
	}
	return s.setSetting(ctx, settingTheme, name)
}

// Timezone returns the zone timestamps are displayed in.
//
// UTC is the default rather than the server's local zone: a lab spans machines
// in different places, and a timestamp is only comparable across them if
// everyone is reading it in the same zone. An explicit UTC is unambiguous;
// "whatever the server happens to be set to" is not.
func (s *Store) Timezone(ctx context.Context) (string, error) {
	v, err := s.setting(ctx, settingTimezone)
	if err != nil || strings.TrimSpace(v) == "" {
		return "UTC", nil
	}
	if _, err := time.LoadLocation(v); err != nil {
		return "UTC", nil
	}
	return v, nil
}

// SetTimezone changes the display zone. The name is validated against the IANA
// database rather than stored blindly, so an unusable value cannot be saved.
func (s *Store) SetTimezone(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "UTC"
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("unknown time zone %q — use an IANA name such as Europe/London", name)
	}
	return s.setSetting(ctx, settingTimezone, name)
}
