package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Collection states.
//
// Probing is deliberately its own state rather than folded into transferring:
// asking how big a file is happens before any bytes move, and an operator
// pulling a 40 GB file by mistake should see that before the transfer starts.
const (
	CollectQueued       = "queued"
	CollectProbing      = "probing"
	CollectTransferring = "transferring"
	CollectDone         = "done"
	CollectFailed       = "failed"
	CollectExpired      = "expired"
)

// CollectRetention is how long a collected file is kept before deletion.
const CollectRetention = 7 * 24 * time.Hour

// EventCollect records a completed collection on the device timeline.
const EventCollect = "file_collected"

// Collection is one request to pull a file off a device.
type Collection struct {
	ID             string `json:"id"`
	DeviceID       string `json:"device_id"`
	DeviceHostname string `json:"device_hostname"`
	Path           string `json:"path"`
	Filename       string `json:"filename"`
	State          string `json:"state"`
	Size           int64  `json:"size"`
	Received       int64  `json:"received"`
	// Progress is how far the transfer has got, 0-100. Computed here rather
	// than in the browser so there is one implementation of it.
	Progress   int        `json:"progress"`
	SHA256     string     `json:"sha256,omitempty"`
	Error      string     `json:"error,omitempty"`
	Copied     bool       `json:"copied"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// RequestCollection queues a file pull from a device.
func (s *Store) RequestCollection(ctx context.Context, deviceID, path, createdBy string) (Collection, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Collection{}, errors.New("a file path is required")
	}

	device, err := s.GetDevice(ctx, deviceID)
	if err != nil {
		return Collection{}, err
	}
	if device.RetiredAt != nil {
		return Collection{}, fmt.Errorf("%s is being retired", device.Hostname)
	}

	id, err := randomToken()
	if err != nil {
		return Collection{}, err
	}
	now := time.Now()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO collections (id, device_id, device_hostname, path, state, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, deviceID, device.Hostname, path, CollectQueued, createdBy, now.Unix(),
	); err != nil {
		return Collection{}, fmt.Errorf("queue collection: %w", err)
	}

	return Collection{
		ID: id, DeviceID: deviceID, DeviceHostname: device.Hostname, Path: path,
		State: CollectQueued, CreatedBy: createdBy, CreatedAt: now,
	}, nil
}

// ClaimCollections hands a device its queued file requests and marks them
// probing, so each is delivered exactly once.
func (s *Store) ClaimCollections(ctx context.Context, deviceID string) ([]Collection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim collections: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, path FROM collections WHERE device_id = ? AND state = ? ORDER BY created_at`,
		deviceID, CollectQueued)
	if err != nil {
		return nil, fmt.Errorf("read queued collections: %w", err)
	}

	var out []Collection
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Path); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan collection: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	for _, c := range out {
		if _, err := tx.ExecContext(ctx,
			`UPDATE collections SET state = ? WHERE id = ?`, CollectProbing, c.ID); err != nil {
			return nil, fmt.Errorf("mark collection probing: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim collections: %w", err)
	}
	return out, nil
}

// RecordCollectionSize stores what the agent found when it looked at the file.
// A failure here — missing, unreadable — ends the collection before any
// transfer is attempted.
func (s *Store) RecordCollectionSize(ctx context.Context, id, deviceID, filename string, size int64, copied bool, probeErr string) error {
	if probeErr != "" {
		return s.FailCollection(ctx, id, deviceID, probeErr)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE collections SET state = ?, size = ?, filename = ?, copied = ?
		 WHERE id = ? AND device_id = ? AND state IN (?, ?)`,
		CollectTransferring, size, filename, boolToInt(copied), id, deviceID, CollectProbing, CollectQueued)
	if err != nil {
		return fmt.Errorf("record collection size: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateCollectionProgress records bytes received so the dashboard can show a
// transfer moving. Called periodically during the upload, not per chunk.
func (s *Store) UpdateCollectionProgress(ctx context.Context, id string, received int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE collections SET received = ? WHERE id = ?`, received, id)
	if err != nil {
		return fmt.Errorf("update collection progress: %w", err)
	}
	return nil
}

// CompleteCollection marks a transfer finished and starts its retention clock.
func (s *Store) CompleteCollection(ctx context.Context, id, deviceID, sha256 string, received int64) error {
	now := time.Now()
	expires := now.Add(CollectRetention)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin complete collection: %w", err)
	}
	defer tx.Rollback()

	var path string
	err = tx.QueryRowContext(ctx,
		`SELECT path FROM collections WHERE id = ? AND device_id = ?`, id, deviceID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read collection: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE collections SET state = ?, sha256 = ?, received = ?, finished_at = ?, expires_at = ?
		 WHERE id = ?`,
		CollectDone, sha256, received, now.Unix(), expires.Unix(), id,
	); err != nil {
		return fmt.Errorf("complete collection: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_events (device_id, ts, kind, detail) VALUES (?, ?, ?, ?)`,
		deviceID, now.Unix(), EventCollect, fmt.Sprintf("%s (%d bytes)", path, received),
	); err != nil {
		return fmt.Errorf("insert collect event: %w", err)
	}
	return tx.Commit()
}

// FailCollection records why a pull did not happen.
func (s *Store) FailCollection(ctx context.Context, id, deviceID, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE collections SET state = ?, error = ?, finished_at = ? WHERE id = ? AND device_id = ?`,
		CollectFailed, reason, time.Now().Unix(), id, deviceID)
	if err != nil {
		return fmt.Errorf("fail collection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetCollection returns one collection.
func (s *Store) GetCollection(ctx context.Context, id string) (Collection, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, device_id, device_hostname, path, COALESCE(filename, ''), state,
		        COALESCE(size, 0), COALESCE(received, 0), COALESCE(sha256, ''), COALESCE(error, ''),
		        COALESCE(copied, 0), created_by, created_at, finished_at, expires_at
		 FROM collections WHERE id = ?`, id)
	c, err := scanCollection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Collection{}, ErrNotFound
	}
	return c, err
}

// DeviceCollections lists a device's pulled files, newest first.
func (s *Store) DeviceCollections(ctx context.Context, deviceID string, limit int) ([]Collection, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, device_id, device_hostname, path, COALESCE(filename, ''), state,
		        COALESCE(size, 0), COALESCE(received, 0), COALESCE(sha256, ''), COALESCE(error, ''),
		        COALESCE(copied, 0), created_by, created_at, finished_at, expires_at
		 FROM collections WHERE device_id = ? ORDER BY created_at DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()

	out := []Collection{}
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ExpiredCollections returns collections past their retention, so the caller
// can delete the files before the rows.
func (s *Store) ExpiredCollections(ctx context.Context) ([]Collection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, device_id, device_hostname, path, COALESCE(filename, ''), state,
		        COALESCE(size, 0), COALESCE(received, 0), COALESCE(sha256, ''), COALESCE(error, ''),
		        COALESCE(copied, 0), created_by, created_at, finished_at, expires_at
		 FROM collections
		 WHERE state = ? AND expires_at IS NOT NULL AND expires_at <= ?`,
		CollectDone, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list expired collections: %w", err)
	}
	defer rows.Close()

	out := []Collection{}
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkCollectionExpired records that a collected file has been deleted. The row
// is kept: knowing a file was pulled, by whom, and that it has since been
// removed is the audit trail.
func (s *Store) MarkCollectionExpired(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE collections SET state = ?, sha256 = '' WHERE id = ?`, CollectExpired, id)
	if err != nil {
		return fmt.Errorf("mark collection expired: %w", err)
	}
	return nil
}

// DeleteCollection removes the record entirely.
func (s *Store) DeleteCollection(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM collections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepStalledCollections fails transfers whose agent never came back, so
// nothing sits mid-transfer forever.
func (s *Store) SweepStalledCollections(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE collections SET state = ?, error = 'agent never completed the transfer',
		 finished_at = ? WHERE state IN (?, ?) AND created_at < ?`,
		CollectFailed, time.Now().Unix(), CollectProbing, CollectTransferring, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sweep stalled collections: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanCollection(row scanner) (Collection, error) {
	var (
		c                 Collection
		created           int64
		finished, expires sql.NullInt64
		copied            int
	)
	if err := row.Scan(&c.ID, &c.DeviceID, &c.DeviceHostname, &c.Path, &c.Filename, &c.State,
		&c.Size, &c.Received, &c.SHA256, &c.Error, &copied, &c.CreatedBy, &created,
		&finished, &expires); err != nil {
		return Collection{}, err
	}
	c.Copied = copied != 0
	switch {
	case c.Size <= 0:
		c.Progress = 0
	case c.Received >= c.Size:
		c.Progress = 100
	default:
		c.Progress = int(c.Received * 100 / c.Size)
	}
	c.CreatedAt = time.Unix(created, 0)
	if finished.Valid {
		t := time.Unix(finished.Int64, 0)
		c.FinishedAt = &t
	}
	if expires.Valid {
		t := time.Unix(expires.Int64, 0)
		c.ExpiresAt = &t
	}
	return c, nil
}
