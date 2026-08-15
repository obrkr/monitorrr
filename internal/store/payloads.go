package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Payload is a file uploaded to be pushed to devices alongside a script — an
// installer, a package, a config bundle.
//
// Only metadata lives in the database. The bytes are on disk: an MSI or a .deb
// is megabytes, and SQLite is a poor place for them when the only access
// pattern is "stream this whole file to one agent".
type Payload struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	// UsedBy names the scripts currently attached to this payload, so deleting
	// one that is still wired up can warn rather than silently break dispatch.
	UsedBy []string `json:"used_by,omitempty"`
}

// AddPayload records an uploaded file. The caller has already written the bytes
// and computed the digest.
func (s *Store) AddPayload(ctx context.Context, id, filename string, size int64, sha256, createdBy string) (Payload, error) {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return Payload{}, errors.New("a filename is required")
	}
	now := time.Now()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO payloads (id, filename, size, sha256, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, filename, size, sha256, now.Unix(), createdBy,
	); err != nil {
		return Payload{}, fmt.Errorf("insert payload: %w", err)
	}
	return Payload{
		ID: id, Filename: filename, Size: size, SHA256: sha256,
		CreatedAt: now, CreatedBy: createdBy,
	}, nil
}

// GetPayload returns one payload's metadata.
func (s *Store) GetPayload(ctx context.Context, id string) (Payload, error) {
	var (
		p       Payload
		created int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, filename, size, sha256, created_at, created_by FROM payloads WHERE id = ?`, id,
	).Scan(&p.ID, &p.Filename, &p.Size, &p.SHA256, &created, &p.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Payload{}, ErrNotFound
	}
	if err != nil {
		return Payload{}, fmt.Errorf("read payload: %w", err)
	}
	p.CreatedAt = time.Unix(created, 0)
	return p, nil
}

// ListPayloads returns every uploaded file, newest first, each with the scripts
// that reference it.
func (s *Store) ListPayloads(ctx context.Context) ([]Payload, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.filename, p.size, p.sha256, p.created_at, p.created_by,
		        COALESCE(GROUP_CONCAT(s.name, ', '), '')
		 FROM payloads p LEFT JOIN scripts s ON s.payload_id = p.id
		 GROUP BY p.id ORDER BY p.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list payloads: %w", err)
	}
	defer rows.Close()

	payloads := []Payload{}
	for rows.Next() {
		var (
			p       Payload
			created int64
			usedBy  string
		)
		if err := rows.Scan(&p.ID, &p.Filename, &p.Size, &p.SHA256, &created, &p.CreatedBy, &usedBy); err != nil {
			return nil, fmt.Errorf("scan payload: %w", err)
		}
		p.CreatedAt = time.Unix(created, 0)
		if usedBy != "" {
			p.UsedBy = strings.Split(usedBy, ", ")
		}
		payloads = append(payloads, p)
	}
	return payloads, rows.Err()
}

// DeletePayload removes the metadata row. The caller deletes the file itself.
func (s *Store) DeletePayload(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM payloads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete payload: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AttachPayload links a payload to a script, or clears it when payloadID is
// empty. Every dispatch of that script then pushes the file with it.
func (s *Store) AttachPayload(ctx context.Context, scriptID, payloadID string) error {
	if payloadID != "" {
		if _, err := s.GetPayload(ctx, payloadID); err != nil {
			return err
		}
	}
	var arg any
	if payloadID != "" {
		arg = payloadID
	}
	res, err := s.db.ExecContext(ctx, `UPDATE scripts SET payload_id = ? WHERE id = ?`, arg, scriptID)
	if err != nil {
		return fmt.Errorf("attach payload: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PayloadForJob returns the payload a dispatched job needs, if any. The job
// carries its own snapshot of the id, so editing or detaching the script later
// cannot change what an in-flight job receives.
func (s *Store) PayloadForJob(ctx context.Context, jobID, deviceID string) (Payload, error) {
	var payloadID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT payload_id FROM jobs WHERE id = ? AND device_id = ?`, jobID, deviceID).Scan(&payloadID)
	if errors.Is(err, sql.ErrNoRows) {
		return Payload{}, ErrNotFound
	}
	if err != nil {
		return Payload{}, fmt.Errorf("read job payload: %w", err)
	}
	if !payloadID.Valid || payloadID.String == "" {
		return Payload{}, ErrNotFound
	}
	return s.GetPayload(ctx, payloadID.String)
}
