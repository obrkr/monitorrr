// Package store owns all persistence. Every query here is vanilla SQL held
// behind this package so the backing database can be swapped (SQLite today,
// Postgres if the fleet or the metric set ever outgrows it) without touching
// the server or agent.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS devices (
  id               TEXT PRIMARY KEY,
  token_hash       TEXT NOT NULL,
  hostname         TEXT NOT NULL,
  os               TEXT NOT NULL,
  arch             TEXT NOT NULL,
  agent_version    TEXT NOT NULL DEFAULT '',
  local_ips        TEXT NOT NULL DEFAULT '',
  remote_ip        TEXT NOT NULL DEFAULT '',
  interval_override INTEGER,
  status           TEXT NOT NULL DEFAULT 'online',
  enrolled_at      INTEGER NOT NULL,
  last_seen        INTEGER NOT NULL,
  -- Set when retirement is requested; the device stays visible as "retiring"
  -- until its agent checks in, uninstalls itself, and acknowledges.
  retired_at       INTEGER,
  -- Capabilities the agent advertises, comma separated. Empty means an agent
  -- old enough to predate capability advertisement.
  features         TEXT NOT NULL DEFAULT '',
  -- Check-ins received since retirement was requested. A climbing count means
  -- the agent is alive and ignoring the instruction.
  retire_checkins  INTEGER NOT NULL DEFAULT 0,
  -- The address the device appears as on the internet, reported by the agent.
  -- Distinct from remote_ip, which is only the source address of the
  -- connection and is private whenever the agent shares our network.
  public_ip        TEXT NOT NULL DEFAULT '',
  -- Free-form labels for targeting, comma separated and normalised lowercase.
  -- A text column rather than a join table: a home lab has tens of devices and
  -- a handful of tags, and this keeps every query a single statement.
  tags             TEXT NOT NULL DEFAULT ''
);

-- Append-only history. We deliberately do NOT write a row per heartbeat: only
-- transitions land here, which keeps the table tiny and makes it readable as an
-- actual timeline rather than a firehose.
CREATE TABLE IF NOT EXISTS device_events (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  ts        INTEGER NOT NULL,
  kind      TEXT NOT NULL,
  detail    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_events_device_ts ON device_events(device_id, ts DESC);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- Files pushed to devices alongside a script: installers, packages, bundles.
-- Only metadata here; the bytes live on disk beside the database.
CREATE TABLE IF NOT EXISTS payloads (
  id         TEXT PRIMARY KEY,
  filename   TEXT NOT NULL,
  size       INTEGER NOT NULL,
  sha256     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS scripts (
  id              TEXT PRIMARY KEY,
  name            TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  interpreter     TEXT NOT NULL,
  content         TEXT NOT NULL,
  sha256          TEXT NOT NULL,
  timeout_seconds INTEGER NOT NULL DEFAULT 300,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  -- Optional file pushed to the device before the script runs.
  payload_id      TEXT REFERENCES payloads(id) ON DELETE SET NULL
);

-- One row per (script, device) dispatch. The script body and hash are snapshot
-- here rather than joined from scripts: the audit trail must still show exactly
-- what ran on a machine after the script is edited or deleted.
CREATE TABLE IF NOT EXISTS jobs (
  id              TEXT PRIMARY KEY,
  script_id       TEXT REFERENCES scripts(id) ON DELETE SET NULL,
  script_name     TEXT NOT NULL,
  script_content  TEXT NOT NULL,
  script_sha256   TEXT NOT NULL,
  interpreter     TEXT NOT NULL,
  timeout_seconds INTEGER NOT NULL,
  device_id       TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  device_hostname TEXT NOT NULL,
  state           TEXT NOT NULL,
  created_by      TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  dispatched_at   INTEGER,
  finished_at     INTEGER,
  exit_code       INTEGER,
  stdout          TEXT NOT NULL DEFAULT '',
  stderr          TEXT NOT NULL DEFAULT '',
  error           TEXT NOT NULL DEFAULT '',
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  truncated       INTEGER NOT NULL DEFAULT 0,
  -- Snapshot of the attached file, so detaching or replacing it later cannot
  -- change what an in-flight job receives or what the audit trail says ran.
  payload_id      TEXT,
  payload_name    TEXT NOT NULL DEFAULT '',
  payload_sha256  TEXT NOT NULL DEFAULT ''
);
-- Files pulled off a device. The bytes land on disk beside the database; this
-- row tracks the request, its progress, and when the file is due for deletion.
CREATE TABLE IF NOT EXISTS collections (
  id              TEXT PRIMARY KEY,
  device_id       TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  device_hostname TEXT NOT NULL,
  path            TEXT NOT NULL,
  filename        TEXT NOT NULL DEFAULT '',
  state           TEXT NOT NULL,
  size            INTEGER NOT NULL DEFAULT 0,
  received        INTEGER NOT NULL DEFAULT 0,
  sha256          TEXT NOT NULL DEFAULT '',
  error           TEXT NOT NULL DEFAULT '',
  -- Whether the agent had to copy the file aside before reading it, which is
  -- how a locked or in-use file gets collected.
  copied          INTEGER NOT NULL DEFAULT 0,
  created_by      TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  finished_at     INTEGER,
  expires_at      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_collections_device ON collections(device_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_collections_pending ON collections(device_id, state);

CREATE INDEX IF NOT EXISTS idx_jobs_pending ON jobs(device_id, state);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at DESC);
`

// Event kinds written to device_events.
const (
	EventEnrolled  = "enrolled"
	EventOnline    = "online"
	EventOffline   = "offline"
	EventIPChanged = "ip_changed"
	EventUpgraded  = "version_changed"
	EventRenamed   = "hostname_changed"
	EventRetiring  = "retire_requested"
	EventRetired   = "retired"
)

// StatusRetired is the terminal device status, set once an agent has confirmed
// it uninstalled itself.
const StatusRetired = "retired"

// Setting keys.
const (
	settingEnrollToken     = "enroll_token"
	settingDefaultInterval = "default_interval"
	settingAutoUpdate      = "auto_update"
)

// DefaultInterval is the check-in period seeded on first run, in seconds.
const DefaultInterval = 60

// Device is one enrolled endpoint.
type Device struct {
	ID           string    `json:"id"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	AgentVersion string    `json:"agent_version"`
	LocalIPs     []string  `json:"local_ips"`
	RemoteIP     string    `json:"remote_ip"`
	PublicIP     string    `json:"public_ip"`
	Interval     int       `json:"interval_seconds"`
	Status       string    `json:"status"`
	EnrolledAt   time.Time `json:"enrolled_at"`
	LastSeen     time.Time `json:"last_seen"`
	// RetiredAt is set as soon as retirement is requested, before the agent has
	// acknowledged. Status becomes "retired" only once it has.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
	// Features the agent advertises. Empty means an agent predating capability
	// advertisement, which cannot act on newer instructions.
	Features []string `json:"features"`
	// RetireCheckins counts heartbeats received since retirement was requested.
	RetireCheckins int `json:"retire_checkins"`
	// Tags are operator-assigned labels used to target groups of devices.
	Tags []string `json:"tags"`
}

// SupportsRetire reports whether this agent will act on a retire instruction.
func (d Device) SupportsRetire() bool {
	for _, f := range d.Features {
		if f == "retire" {
			return true
		}
	}
	return false
}

// Event is one entry from a device's timeline.
type Event struct {
	DeviceID string    `json:"device_id"`
	Hostname string    `json:"hostname"`
	TS       time.Time `json:"ts"`
	Kind     string    `json:"kind"`
	Detail   string    `json:"detail"`
}

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open connects to the SQLite file at path, applies the schema, and seeds
// first-run settings. WAL mode lets the dashboard read while agents check in.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// SQLite takes a single writer; more connections just create lock contention.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.seed(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate adds columns introduced after a database was first created. The
// schema above uses CREATE TABLE IF NOT EXISTS, which does nothing for an
// existing table, so new columns have to be added explicitly.
func (s *Store) migrate() error {
	columns := []struct{ table, column, ddl string }{
		{"devices", "retired_at", "ALTER TABLE devices ADD COLUMN retired_at INTEGER"},
		{"devices", "features", "ALTER TABLE devices ADD COLUMN features TEXT NOT NULL DEFAULT ''"},
		{"devices", "retire_checkins", "ALTER TABLE devices ADD COLUMN retire_checkins INTEGER NOT NULL DEFAULT 0"},
		{"devices", "public_ip", "ALTER TABLE devices ADD COLUMN public_ip TEXT NOT NULL DEFAULT ''"},
		{"devices", "tags", "ALTER TABLE devices ADD COLUMN tags TEXT NOT NULL DEFAULT ''"},
		{"scripts", "payload_id", "ALTER TABLE scripts ADD COLUMN payload_id TEXT"},
		{"jobs", "payload_id", "ALTER TABLE jobs ADD COLUMN payload_id TEXT"},
		{"jobs", "payload_name", "ALTER TABLE jobs ADD COLUMN payload_name TEXT NOT NULL DEFAULT ''"},
		{"jobs", "payload_sha256", "ALTER TABLE jobs ADD COLUMN payload_sha256 TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range columns {
		var n int
		err := s.db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n)
		if err != nil {
			return fmt.Errorf("inspect %s.%s: %w", c.table, c.column, err)
		}
		if n > 0 {
			continue
		}
		if _, err := s.db.Exec(c.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) seed() error {
	ctx := context.Background()
	if _, err := s.setting(ctx, settingEnrollToken); errors.Is(err, ErrNotFound) {
		tok, err := randomToken()
		if err != nil {
			return err
		}
		if err := s.setSetting(ctx, settingEnrollToken, tok); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if _, err := s.setting(ctx, settingDefaultInterval); errors.Is(err, ErrNotFound) {
		return s.setSetting(ctx, settingDefaultInterval, fmt.Sprint(DefaultInterval))
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Store) setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read setting %q: %w", key, err)
	}
	return v, nil
}

func (s *Store) setSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write setting %q: %w", key, err)
	}
	return nil
}

// EnrollToken returns the shared secret an agent must present to enroll.
func (s *Store) EnrollToken(ctx context.Context) (string, error) {
	return s.setting(ctx, settingEnrollToken)
}

// RotateEnrollToken issues a new enrollment secret. Already-enrolled agents are
// unaffected: they authenticate with their own per-device token.
func (s *Store) RotateEnrollToken(ctx context.Context) (string, error) {
	tok, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := s.setSetting(ctx, settingEnrollToken, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// AutoUpdateEnabled reports whether agents should replace themselves when the
// server is serving a different build. Defaults to on: the alternative is a
// fleet that silently drifts, which is exactly the problem it exists to solve.
func (s *Store) AutoUpdateEnabled(ctx context.Context) (bool, error) {
	v, err := s.setting(ctx, settingAutoUpdate)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// SetAutoUpdate turns fleet-wide self-update on or off.
func (s *Store) SetAutoUpdate(ctx context.Context, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return s.setSetting(ctx, settingAutoUpdate, v)
}

// DefaultCheckinInterval returns the fleet-wide check-in period in seconds.
func (s *Store) DefaultCheckinInterval(ctx context.Context) (int, error) {
	v, err := s.setting(ctx, settingDefaultInterval)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return DefaultInterval, nil
	}
	return n, nil
}

// SetDefaultCheckinInterval changes the fleet-wide check-in period. Agents pick
// it up on their next heartbeat, so a fleet on a 60s interval fully converges
// within 60 seconds.
func (s *Store) SetDefaultCheckinInterval(ctx context.Context, seconds int) error {
	if seconds < 5 || seconds > 86400 {
		return fmt.Errorf("interval must be between 5 and 86400 seconds, got %d", seconds)
	}
	return s.setSetting(ctx, settingDefaultInterval, fmt.Sprint(seconds))
}

// Enroll registers a new device and returns its ID and plaintext token. The
// token is shown exactly once; only its hash is stored.
func (s *Store) Enroll(ctx context.Context, hostname, osName, arch, version, remoteIP string) (id, token string, err error) {
	id, err = randomToken()
	if err != nil {
		return "", "", err
	}
	token, err = randomToken()
	if err != nil {
		return "", "", err
	}

	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("begin enroll: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO devices (id, token_hash, hostname, os, arch, agent_version, remote_ip, status, enrolled_at, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'online', ?, ?)`,
		id, hashToken(token), hostname, osName, arch, version, remoteIP, now, now,
	); err != nil {
		return "", "", fmt.Errorf("insert device: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_events (device_id, ts, kind, detail) VALUES (?, ?, ?, ?)`,
		id, now, EventEnrolled, fmt.Sprintf("%s (%s/%s)", hostname, osName, arch),
	); err != nil {
		return "", "", fmt.Errorf("insert enroll event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("commit enroll: %w", err)
	}
	return id, token, nil
}

// Authenticate verifies a device ID and token pair.
func (s *Store) Authenticate(ctx context.Context, id, token string) error {
	var want string
	err := s.db.QueryRowContext(ctx, `SELECT token_hash FROM devices WHERE id = ?`, id).Scan(&want)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read device token: %w", err)
	}
	if hashToken(token) != want {
		return ErrNotFound
	}
	return nil
}

// Checkin records a heartbeat and returns the interval the agent should use,
// plus whether this device has been retired and should uninstall itself.
// Only genuine changes are written to the event timeline.
func (s *Store) Checkin(ctx context.Context, id, hostname, version, remoteIP, publicIP string, localIPs, features []string) (interval int, retire bool, err error) {
	now := time.Now().Unix()
	ips := strings.Join(localIPs, ",")
	feat := strings.Join(features, ",")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin checkin: %w", err)
	}
	defer tx.Rollback()

	var (
		prevStatus, prevHostname, prevVersion, prevIPs, prevRemote string
		override, retiredAt                                        sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT status, hostname, agent_version, local_ips, remote_ip, interval_override, retired_at
		 FROM devices WHERE id = ?`, id,
	).Scan(&prevStatus, &prevHostname, &prevVersion, &prevIPs, &prevRemote, &override, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, fmt.Errorf("read device: %w", err)
	}
	retire = retiredAt.Valid

	// A retiring device keeps its last_seen updated — that is how you see the
	// instruction was delivered — but must not be flipped back to online, or
	// the dashboard would claim a machine being decommissioned is healthy.
	status := "online"
	if retire {
		status = prevStatus
	}
	// Counting check-ins that arrive after retirement was requested is what
	// distinguishes a powered-off machine (count stays put) from one that is
	// alive and not acting on the instruction (count climbs).
	retireIncrement := 0
	if retire {
		retireIncrement = 1
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET hostname = ?, agent_version = ?, local_ips = ?, remote_ip = ?,
		 public_ip = COALESCE(NULLIF(?, ''), public_ip),
		 status = ?, last_seen = ?, features = ?, retire_checkins = retire_checkins + ?
		 WHERE id = ?`,
		hostname, version, ips, remoteIP, publicIP, status, now, feat, retireIncrement, id,
	); err != nil {
		return 0, false, fmt.Errorf("update device: %w", err)
	}

	addEvent := func(kind, detail string) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO device_events (device_id, ts, kind, detail) VALUES (?, ?, ?, ?)`,
			id, now, kind, detail)
		return err
	}

	// Transitions are noise on a device that is on its way out.
	if !retire {
		if prevStatus != "online" {
			if err := addEvent(EventOnline, "agent resumed check-ins"); err != nil {
				return 0, false, err
			}
		}
		if prevHostname != hostname {
			if err := addEvent(EventRenamed, fmt.Sprintf("%s → %s", prevHostname, hostname)); err != nil {
				return 0, false, err
			}
		}
		if prevVersion != version {
			if err := addEvent(EventUpgraded, fmt.Sprintf("%s → %s", prevVersion, version)); err != nil {
				return 0, false, err
			}
		}
		// prevIPs is empty only on the first check-in after enrollment, where the
		// enrolled event already covers the device appearing. Reporting an address
		// change there would put a redundant line on every new device's timeline.
		if prevIPs != "" && (prevIPs != ips || prevRemote != remoteIP) {
			if err := addEvent(EventIPChanged, fmt.Sprintf("%s (via %s)", ips, remoteIP)); err != nil {
				return 0, false, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit checkin: %w", err)
	}

	if override.Valid && override.Int64 > 0 {
		return int(override.Int64), retire, nil
	}
	interval, err = s.DefaultCheckinInterval(ctx)
	return interval, retire, err
}

// RetireDevice marks a device for decommissioning. The agent is told to
// uninstall itself on its next check-in; until it acknowledges, the device
// stays visible so a machine that is powered off is not silently forgotten.
func (s *Store) RetireDevice(ctx context.Context, id string) error {
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin retire: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE devices SET retired_at = ? WHERE id = ? AND retired_at IS NULL`, now, id)
	if err != nil {
		return fmt.Errorf("retire device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either no such device, or retirement was already requested. Confirm
		// which, so the caller can report it accurately.
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE id = ?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("check device: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}
		return nil // already retiring; requesting again is harmless
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_events (device_id, ts, kind, detail) VALUES (?, ?, ?, ?)`,
		id, now, EventRetiring, "waiting for the agent to check in and uninstall itself",
	); err != nil {
		return fmt.Errorf("insert retire event: %w", err)
	}

	// Anything still queued would be pointless work on a machine on its way out.
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, finished_at = ?, error = 'cancelled: device retired'
		 WHERE device_id = ? AND state = ?`, JobLost, now, id, JobQueued); err != nil {
		return fmt.Errorf("cancel queued jobs: %w", err)
	}

	return tx.Commit()
}

// CompleteRetirement records an agent's confirmation that it uninstalled
// itself. The record is kept, not deleted: knowing a machine was decommissioned
// and when is the point of retiring it rather than just deleting the row.
func (s *Store) CompleteRetirement(ctx context.Context, id string) error {
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin complete retirement: %w", err)
	}
	defer tx.Rollback()

	var (
		status    string
		retiredAt sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, `SELECT status, retired_at FROM devices WHERE id = ?`, id).
		Scan(&status, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read device: %w", err)
	}
	if !retiredAt.Valid {
		// Nobody asked this device to retire; it does not get to decide.
		return ErrNotFound
	}
	// A supervisor may restart the agent inside the teardown window, so the same
	// acknowledgement can legitimately arrive twice. Treat the repeat as a no-op
	// rather than writing a second "retired" line to the timeline.
	if status == StatusRetired {
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET status = ? WHERE id = ?`, StatusRetired, id); err != nil {
		return fmt.Errorf("complete retirement: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_events (device_id, ts, kind, detail) VALUES (?, ?, ?, ?)`,
		id, now, EventRetired, "agent confirmed it uninstalled itself",
	); err != nil {
		return fmt.Errorf("insert retired event: %w", err)
	}
	return tx.Commit()
}

// ListDevices returns every enrolled device, most recently seen first.
func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	def, err := s.DefaultCheckinInterval(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, hostname, os, arch, agent_version, local_ips, remote_ip, public_ip,
		        interval_override, status, enrolled_at, last_seen, retired_at,
		        features, retire_checkins, tags
		 FROM devices ORDER BY last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	// Non-nil so the JSON API emits [] rather than null on an empty fleet.
	devices := []Device{}
	for rows.Next() {
		var (
			d                  Device
			ips, feat, tags    string
			override, retired  sql.NullInt64
			enrolled, lastSeen int64
		)
		if err := rows.Scan(&d.ID, &d.Hostname, &d.OS, &d.Arch, &d.AgentVersion, &ips,
			&d.RemoteIP, &d.PublicIP, &override, &d.Status, &enrolled, &lastSeen, &retired,
			&feat, &d.RetireCheckins, &tags); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		if retired.Valid {
			t := time.Unix(retired.Int64, 0)
			d.RetiredAt = &t
		}
		// Non-nil so the JSON API emits [] rather than null.
		d.Features = []string{}
		if feat != "" {
			d.Features = strings.Split(feat, ",")
		}
		d.Tags = []string{}
		if tags != "" {
			d.Tags = strings.Split(tags, ",")
		}
		if ips != "" {
			d.LocalIPs = strings.Split(ips, ",")
		}
		d.Interval = def
		if override.Valid && override.Int64 > 0 {
			d.Interval = int(override.Int64)
		}
		d.EnrolledAt = time.Unix(enrolled, 0)
		d.LastSeen = time.Unix(lastSeen, 0)
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// RecentEvents returns the newest entries across the whole fleet.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.device_id, d.hostname, e.ts, e.kind, e.detail
		 FROM device_events e JOIN devices d ON d.id = e.device_id
		 ORDER BY e.ts DESC, e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	events := []Event{}
	for rows.Next() {
		var (
			e  Event
			ts int64
		)
		if err := rows.Scan(&e.DeviceID, &e.Hostname, &ts, &e.Kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.TS = time.Unix(ts, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}

// DeleteDevice removes a device and its history. The agent will re-enroll if it
// is still running, which is the intended way to reset a machine's identity.
func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepOffline marks devices whose last heartbeat is older than grace times
// their interval. A grace of 3 absorbs a single dropped check-in without
// flapping the dashboard.
func (s *Store) SweepOffline(ctx context.Context, grace int) (int, error) {
	def, err := s.DefaultCheckinInterval(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin sweep: %w", err)
	}
	defer tx.Rollback()

	// Retiring devices are excluded: a machine on its way out going quiet is the
	// expected outcome, not an incident.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, last_seen, COALESCE(interval_override, ?)
		 FROM devices WHERE status = 'online' AND retired_at IS NULL`, def)
	if err != nil {
		return 0, fmt.Errorf("scan for stale devices: %w", err)
	}

	var stale []string
	for rows.Next() {
		var (
			id                 string
			lastSeen, interval int64
		)
		if err := rows.Scan(&id, &lastSeen, &interval); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan stale device: %w", err)
		}
		if now-lastSeen > interval*int64(grace) {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET status = 'offline' WHERE id = ?`, id); err != nil {
			return 0, fmt.Errorf("mark offline: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO device_events (device_id, ts, kind, detail) VALUES (?, ?, ?, ?)`,
			id, now, EventOffline, fmt.Sprintf("missed %d consecutive check-ins", grace),
		); err != nil {
			return 0, fmt.Errorf("insert offline event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit sweep: %w", err)
	}
	return len(stale), nil
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}
