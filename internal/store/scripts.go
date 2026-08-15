package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Supported interpreters.
const (
	InterpreterSh         = "sh"
	InterpreterPowerShell = "powershell"
)

// Job states. A job is queued until an agent collects it on a check-in, running
// until the result is posted, then done. Lost covers an agent that took the job
// and never came back — a machine rebooted mid-script, say.
const (
	JobQueued  = "queued"
	JobRunning = "running"
	JobDone    = "done"
	JobLost    = "lost"
)

// EventJob records a completed execution on the device timeline.
const EventJob = "job"

// maxOutput caps each captured stream. The agent truncates first; this is the
// backstop that keeps a runaway script from bloating the database.
const maxOutput = 64 * 1024

// DefaultJobTimeout is the per-job execution limit in seconds.
const DefaultJobTimeout = 300

// Script is a stored shell or PowerShell script.
type Script struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Interpreter string    `json:"interpreter"`
	Content     string    `json:"content"`
	SHA256      string    `json:"sha256"`
	TimeoutSecs int       `json:"timeout_seconds"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// PayloadID is an optional file pushed to the device before the script runs.
	PayloadID       string `json:"payload_id,omitempty"`
	PayloadFilename string `json:"payload_filename,omitempty"`
	PayloadSize     int64  `json:"payload_size,omitempty"`
	PayloadSHA256   string `json:"payload_sha256,omitempty"`
}

// Job is one dispatch of a script to one device.
type Job struct {
	ID             string     `json:"id"`
	ScriptID       string     `json:"script_id"`
	ScriptName     string     `json:"script_name"`
	ScriptSHA256   string     `json:"script_sha256"`
	Interpreter    string     `json:"interpreter"`
	TimeoutSecs    int        `json:"timeout_seconds"`
	DeviceID       string     `json:"device_id"`
	DeviceHostname string     `json:"device_hostname"`
	State          string     `json:"state"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	DispatchedAt   *time.Time `json:"dispatched_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	Stdout         string     `json:"stdout"`
	Stderr         string     `json:"stderr"`
	Error          string     `json:"error"`
	DurationMS     int64      `json:"duration_ms"`
	Truncated      bool       `json:"truncated"`
	PayloadName    string     `json:"payload_name,omitempty"`
	PayloadSHA256  string     `json:"payload_sha256,omitempty"`
	// Content is populated only by GetJob — it is the snapshot of what actually
	// ran, which is the point of the audit trail.
	Content string `json:"content,omitempty"`
}

// InterpreterSupports reports whether an interpreter can run on a given GOOS.
// This is what stops a PowerShell script being dispatched to a Debian VM.
func InterpreterSupports(interpreter, goos string) bool {
	switch interpreter {
	case InterpreterSh:
		return goos == "linux" || goos == "darwin"
	case InterpreterPowerShell:
		return goos == "windows"
	default:
		return false
	}
}

// ValidInterpreter reports whether the interpreter is one we support at all.
func ValidInterpreter(interpreter string) bool {
	return interpreter == InterpreterSh || interpreter == InterpreterPowerShell
}

// SaveScript creates a script, or updates it when id is non-empty.
func (s *Store) SaveScript(ctx context.Context, id, name, description, interpreter, content string, timeout int) (Script, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Script{}, errors.New("script name is required")
	}
	if !ValidInterpreter(interpreter) {
		return Script{}, fmt.Errorf("unsupported interpreter %q", interpreter)
	}
	if strings.TrimSpace(content) == "" {
		return Script{}, errors.New("script content is required")
	}
	if timeout <= 0 {
		timeout = DefaultJobTimeout
	}
	if timeout > 3600 {
		return Script{}, errors.New("timeout must be 3600 seconds or less")
	}

	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	now := time.Now()

	if id == "" {
		newID, err := randomToken()
		if err != nil {
			return Script{}, err
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO scripts (id, name, description, interpreter, content, sha256, timeout_seconds, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newID, name, description, interpreter, content, digest, timeout, now.Unix(), now.Unix(),
		); err != nil {
			return Script{}, fmt.Errorf("insert script: %w", err)
		}
		return Script{
			ID: newID, Name: name, Description: description, Interpreter: interpreter,
			Content: content, SHA256: digest, TimeoutSecs: timeout,
			CreatedAt: now, UpdatedAt: now,
		}, nil
	}

	// payload_id is deliberately untouched: attaching is its own operation, and
	// editing a script's text must not silently drop its file.
	res, err := s.db.ExecContext(ctx,
		`UPDATE scripts SET name = ?, description = ?, interpreter = ?, content = ?,
		 sha256 = ?, timeout_seconds = ?, updated_at = ? WHERE id = ?`,
		name, description, interpreter, content, digest, timeout, now.Unix(), id)
	if err != nil {
		return Script{}, fmt.Errorf("update script: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Script{}, ErrNotFound
	}
	return s.GetScript(ctx, id)
}

// GetScript returns one script including its body.
func (s *Store) GetScript(ctx context.Context, id string) (Script, error) {
	var (
		sc               Script
		created, updated int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT s.id, s.name, s.description, s.interpreter, s.content, s.sha256, s.timeout_seconds,
		        s.created_at, s.updated_at, COALESCE(s.payload_id, ''),
		        COALESCE(p.filename, ''), COALESCE(p.size, 0), COALESCE(p.sha256, '')
		 FROM scripts s LEFT JOIN payloads p ON p.id = s.payload_id WHERE s.id = ?`, id,
	).Scan(&sc.ID, &sc.Name, &sc.Description, &sc.Interpreter, &sc.Content,
		&sc.SHA256, &sc.TimeoutSecs, &created, &updated,
		&sc.PayloadID, &sc.PayloadFilename, &sc.PayloadSize, &sc.PayloadSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return Script{}, ErrNotFound
	}
	if err != nil {
		return Script{}, fmt.Errorf("read script: %w", err)
	}
	sc.CreatedAt = time.Unix(created, 0)
	sc.UpdatedAt = time.Unix(updated, 0)
	return sc, nil
}

// ListScripts returns all scripts, newest first.
func (s *Store) ListScripts(ctx context.Context) ([]Script, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.name, s.description, s.interpreter, s.content, s.sha256, s.timeout_seconds,
		        s.created_at, s.updated_at, COALESCE(s.payload_id, ''),
		        COALESCE(p.filename, ''), COALESCE(p.size, 0), COALESCE(p.sha256, '')
		 FROM scripts s LEFT JOIN payloads p ON p.id = s.payload_id
		 ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list scripts: %w", err)
	}
	defer rows.Close()

	scripts := []Script{}
	for rows.Next() {
		var (
			sc               Script
			created, updated int64
		)
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.Description, &sc.Interpreter, &sc.Content,
			&sc.SHA256, &sc.TimeoutSecs, &created, &updated,
			&sc.PayloadID, &sc.PayloadFilename, &sc.PayloadSize, &sc.PayloadSHA256); err != nil {
			return nil, fmt.Errorf("scan script: %w", err)
		}
		sc.CreatedAt = time.Unix(created, 0)
		sc.UpdatedAt = time.Unix(updated, 0)
		scripts = append(scripts, sc)
	}
	return scripts, rows.Err()
}

// DeleteScript removes a script. Past jobs keep their own snapshot of the body,
// so history stays intact and readable.
func (s *Store) DeleteScript(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM scripts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete script: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Dispatch queues a script against the given devices and returns the new jobs.
// Devices whose OS cannot run the interpreter are rejected up front: silently
// skipping them would leave the operator believing a fleet-wide run succeeded.
func (s *Store) Dispatch(ctx context.Context, scriptID string, deviceIDs []string, createdBy string) ([]Job, error) {
	if len(deviceIDs) == 0 {
		return nil, errors.New("select at least one device")
	}
	script, err := s.GetScript(ctx, scriptID)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin dispatch: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	jobs := make([]Job, 0, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		var (
			hostname, goos string
			retiredAt      sql.NullInt64
		)
		err := tx.QueryRowContext(ctx, `SELECT hostname, os, retired_at FROM devices WHERE id = ?`, deviceID).
			Scan(&hostname, &goos, &retiredAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("device %s: %w", deviceID, ErrNotFound)
		}
		if err != nil {
			return nil, fmt.Errorf("read device %s: %w", deviceID, err)
		}
		if retiredAt.Valid {
			return nil, fmt.Errorf("%s is being retired and no longer accepts jobs", hostname)
		}
		if !InterpreterSupports(script.Interpreter, goos) {
			return nil, fmt.Errorf("%s runs %s, which cannot execute a %s script", hostname, goos, script.Interpreter)
		}

		id, err := randomToken()
		if err != nil {
			return nil, err
		}
		var payloadArg any
		if script.PayloadID != "" {
			payloadArg = script.PayloadID
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO jobs (id, script_id, script_name, script_content, script_sha256, interpreter,
			 timeout_seconds, device_id, device_hostname, state, created_by, created_at,
			 payload_id, payload_name, payload_sha256)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, script.ID, script.Name, script.Content, script.SHA256, script.Interpreter,
			script.TimeoutSecs, deviceID, hostname, JobQueued, createdBy, now.Unix(),
			payloadArg, script.PayloadFilename, script.PayloadSHA256,
		); err != nil {
			return nil, fmt.Errorf("queue job: %w", err)
		}

		jobs = append(jobs, Job{
			PayloadName:   script.PayloadFilename,
			PayloadSHA256: script.PayloadSHA256,
			ID:            id, ScriptID: script.ID, ScriptName: script.Name, ScriptSHA256: script.SHA256,
			Interpreter: script.Interpreter, TimeoutSecs: script.TimeoutSecs,
			DeviceID: deviceID, DeviceHostname: hostname, State: JobQueued,
			CreatedBy: createdBy, CreatedAt: now,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit dispatch: %w", err)
	}
	return jobs, nil
}

// ClaimJobs hands a device its queued work and marks it running, so a job is
// never delivered twice.
func (s *Store) ClaimJobs(ctx context.Context, deviceID string) ([]Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, script_content, script_sha256, interpreter, timeout_seconds,
		        payload_name, payload_sha256
		 FROM jobs WHERE device_id = ? AND state = ? ORDER BY created_at`, deviceID, JobQueued)
	if err != nil {
		return nil, fmt.Errorf("read queued jobs: %w", err)
	}

	var jobs []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Content, &j.ScriptSHA256, &j.Interpreter, &j.TimeoutSecs,
			&j.PayloadName, &j.PayloadSHA256); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan queued job: %w", err)
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, nil
	}

	now := time.Now().Unix()
	for _, j := range jobs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, dispatched_at = ? WHERE id = ?`, JobRunning, now, j.ID); err != nil {
			return nil, fmt.Errorf("mark job running: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return jobs, nil
}

// CompleteJob records a finished execution. deviceID is checked so an agent can
// only ever write results for its own jobs.
func (s *Store) CompleteJob(ctx context.Context, jobID, deviceID string, exitCode int, stdout, stderr, execErr string, durationMS int64, truncated bool) error {
	stdout, t1 := clamp(stdout)
	stderr, t2 := clamp(stderr)
	truncated = truncated || t1 || t2
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin complete: %w", err)
	}
	defer tx.Rollback()

	var (
		state      string
		scriptName string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT state, script_name FROM jobs WHERE id = ? AND device_id = ?`, jobID, deviceID).
		Scan(&state, &scriptName)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read job: %w", err)
	}
	// A late result for a job already swept as lost is still worth keeping.
	if state == JobDone {
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, finished_at = ?, exit_code = ?, stdout = ?, stderr = ?,
		 error = ?, duration_ms = ?, truncated = ? WHERE id = ?`,
		JobDone, now, exitCode, stdout, stderr, execErr, durationMS, boolToInt(truncated), jobID,
	); err != nil {
		return fmt.Errorf("update job: %w", err)
	}

	detail := fmt.Sprintf("%s exited %d", scriptName, exitCode)
	if execErr != "" {
		detail = fmt.Sprintf("%s failed: %s", scriptName, execErr)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_events (device_id, ts, kind, detail) VALUES (?, ?, ?, ?)`,
		deviceID, now, EventJob, detail,
	); err != nil {
		return fmt.Errorf("insert job event: %w", err)
	}

	return tx.Commit()
}

// ListJobs returns recent jobs without their script bodies.
func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(script_id, ''), script_name, script_sha256, interpreter, timeout_seconds,
		        device_id, device_hostname, state, created_by, created_at, dispatched_at, finished_at,
		        exit_code, stdout, stderr, error, duration_ms, truncated, payload_name, payload_sha256
		 FROM jobs ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
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

// GetJob returns one job including the exact script body that was dispatched.
func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(script_id, ''), script_name, script_sha256, interpreter, timeout_seconds,
		        device_id, device_hostname, state, created_by, created_at, dispatched_at, finished_at,
		        exit_code, stdout, stderr, error, duration_ms, truncated, payload_name, payload_sha256,
		        script_content
		 FROM jobs WHERE id = ?`, id)

	var (
		j                              Job
		created                        int64
		dispatched, finished, exitCode sql.NullInt64
		truncated                      int
	)
	err := row.Scan(&j.ID, &j.ScriptID, &j.ScriptName, &j.ScriptSHA256, &j.Interpreter, &j.TimeoutSecs,
		&j.DeviceID, &j.DeviceHostname, &j.State, &j.CreatedBy, &created, &dispatched, &finished,
		&exitCode, &j.Stdout, &j.Stderr, &j.Error, &j.DurationMS, &truncated,
		&j.PayloadName, &j.PayloadSHA256, &j.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("read job: %w", err)
	}
	fillJobTimes(&j, created, dispatched, finished, exitCode, truncated)
	return j, nil
}

// SweepLostJobs marks jobs whose agent never reported back. The allowance is
// twice the job's own timeout plus a check-in period, so a slow script is not
// mistaken for a dead agent.
func (s *Store) SweepLostJobs(ctx context.Context) (int, error) {
	interval, err := s.DefaultCheckinInterval(ctx)
	if err != nil {
		interval = DefaultInterval
	}
	now := time.Now().Unix()

	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, finished_at = ?, error = 'agent never reported a result'
		 WHERE state = ? AND dispatched_at IS NOT NULL
		   AND ? - dispatched_at > (timeout_seconds * 2 + ?)`,
		JobLost, now, JobRunning, now, interval)
	if err != nil {
		return 0, fmt.Errorf("sweep lost jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PendingJobCount reports how many jobs are queued or running fleet-wide.
func (s *Store) PendingJobCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state IN (?, ?)`, JobQueued, JobRunning).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending jobs: %w", err)
	}
	return n, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanJob(row scanner) (Job, error) {
	var (
		j                              Job
		created                        int64
		dispatched, finished, exitCode sql.NullInt64
		truncated                      int
	)
	if err := row.Scan(&j.ID, &j.ScriptID, &j.ScriptName, &j.ScriptSHA256, &j.Interpreter, &j.TimeoutSecs,
		&j.DeviceID, &j.DeviceHostname, &j.State, &j.CreatedBy, &created, &dispatched, &finished,
		&exitCode, &j.Stdout, &j.Stderr, &j.Error, &j.DurationMS, &truncated,
		&j.PayloadName, &j.PayloadSHA256); err != nil {
		return Job{}, fmt.Errorf("scan job: %w", err)
	}
	fillJobTimes(&j, created, dispatched, finished, exitCode, truncated)
	return j, nil
}

func fillJobTimes(j *Job, created int64, dispatched, finished, exitCode sql.NullInt64, truncated int) {
	j.CreatedAt = time.Unix(created, 0)
	if dispatched.Valid {
		t := time.Unix(dispatched.Int64, 0)
		j.DispatchedAt = &t
	}
	if finished.Valid {
		t := time.Unix(finished.Int64, 0)
		j.FinishedAt = &t
	}
	if exitCode.Valid {
		c := int(exitCode.Int64)
		j.ExitCode = &c
	}
	j.Truncated = truncated != 0
}

func clamp(s string) (string, bool) {
	if len(s) <= maxOutput {
		return s, false
	}
	return s[:maxOutput], true
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
