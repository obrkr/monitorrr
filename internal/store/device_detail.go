package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const deviceColumns = `id, hostname, os, arch, agent_version, local_ips, remote_ip, public_ip,
	interval_override, status, enrolled_at, last_seen, retired_at, features, retire_checkins`

// GetDevice returns a single device.
func (s *Store) GetDevice(ctx context.Context, id string) (Device, error) {
	def, err := s.DefaultCheckinInterval(ctx)
	if err != nil {
		return Device{}, err
	}

	var (
		d                  Device
		ips, feat          string
		override, retired  sql.NullInt64
		enrolled, lastSeen int64
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT `+deviceColumns+` FROM devices WHERE id = ?`, id,
	).Scan(&d.ID, &d.Hostname, &d.OS, &d.Arch, &d.AgentVersion, &ips, &d.RemoteIP, &d.PublicIP,
		&override, &d.Status, &enrolled, &lastSeen, &retired, &feat, &d.RetireCheckins)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("read device: %w", err)
	}

	if ips != "" {
		d.LocalIPs = strings.Split(ips, ",")
	}
	d.Features = []string{}
	if feat != "" {
		d.Features = strings.Split(feat, ",")
	}
	d.Interval = def
	if override.Valid && override.Int64 > 0 {
		d.Interval = int(override.Int64)
	}
	d.EnrolledAt = time.Unix(enrolled, 0)
	d.LastSeen = time.Unix(lastSeen, 0)
	if retired.Valid {
		t := time.Unix(retired.Int64, 0)
		d.RetiredAt = &t
	}
	return d, nil
}

// DeviceEvents returns one device's timeline, newest first.
func (s *Store) DeviceEvents(ctx context.Context, deviceID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.device_id, d.hostname, e.ts, e.kind, e.detail
		 FROM device_events e JOIN devices d ON d.id = e.device_id
		 WHERE e.device_id = ?
		 ORDER BY e.ts DESC, e.id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list device events: %w", err)
	}
	defer rows.Close()

	events := []Event{}
	for rows.Next() {
		var (
			e  Event
			ts int64
		)
		if err := rows.Scan(&e.DeviceID, &e.Hostname, &ts, &e.Kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("scan device event: %w", err)
		}
		e.TS = time.Unix(ts, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}

// DeviceJobs returns one device's run history, newest first.
func (s *Store) DeviceJobs(ctx context.Context, deviceID string, limit int) ([]Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(script_id, ''), script_name, script_sha256, interpreter, timeout_seconds,
		        device_id, device_hostname, state, created_by, created_at, dispatched_at, finished_at,
		        exit_code, stdout, stderr, error, duration_ms, truncated
		 FROM jobs WHERE device_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list device jobs: %w", err)
	}
	defer rows.Close()

	jobs := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
