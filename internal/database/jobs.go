package database

import (
	"database/sql"
	"errors"

	"github.com/peaceful/cloud-console/internal/models"
)

// CreateJob inserts a queued job.
func (d *DB) CreateJob(j *models.Job) error {
	_, err := d.sql.Exec(`
		INSERT INTO jobs (id, kind, target, payload, status, progress, message, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Kind, j.Target, j.Payload, j.Status, j.Progress, j.Message, j.CreatedBy)
	return err
}

// JobPayload returns the stored payload for a job.
func (d *DB) JobPayload(id string) (string, error) {
	var payload string
	err := d.sql.QueryRow(`SELECT payload FROM jobs WHERE id = ?`, id).Scan(&payload)
	return payload, err
}

// WipeJobPayload blanks a job's stored payload.
func (d *DB) WipeJobPayload(id string) error {
	_, err := d.sql.Exec(`UPDATE jobs SET payload = '' WHERE id = ?`, id)
	return err
}

// WipeFinishedJobPayloads clears payloads left by jobs that ended before
// FinishJob started doing it, so old root passwords do not linger.
func (d *DB) WipeFinishedJobPayloads() (int64, error) {
	res, err := d.sql.Exec(`UPDATE jobs SET payload = '' WHERE payload != '' AND status NOT IN ('queued', 'running')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// StartJob marks a job as running.
func (d *DB) StartJob(id string) error {
	_, err := d.sql.Exec(
		`UPDATE jobs SET status = 'running', started_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}

// UpdateJobProgress records progress and a human-readable message.
func (d *DB) UpdateJobProgress(id string, progress int, message string) error {
	_, err := d.sql.Exec(`UPDATE jobs SET progress = ?, message = ? WHERE id = ?`, progress, message, id)
	return err
}

// FinishJob marks a job as done or failed. The payload goes with it: it can
// carry credentials (an instance's root password), and nothing reads it once
// the job is over.
func (d *DB) FinishJob(id, status, message, errMsg string) error {
	_, err := d.sql.Exec(`
		UPDATE jobs SET status = ?, progress = 100, message = ?, error = ?, payload = '', ended_at = CURRENT_TIMESTAMP
		WHERE id = ?`, status, message, errMsg, id)
	return err
}

// GetJob loads a single job.
func (d *DB) GetJob(id string) (*models.Job, error) {
	row := d.sql.QueryRow(`
		SELECT id, kind, target, status, progress, message, error, created_by, created_at, started_at, ended_at
		FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

// ListJobs returns the most recent jobs.
func (d *DB) ListJobs(limit int) ([]models.Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	rows, err := d.sql.Query(`
		SELECT id, kind, target, status, progress, message, error, created_by, created_at, started_at, ended_at
		FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Job
	for rows.Next() {
		j, err := scanJobRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ListActiveJobs returns jobs that are queued or running.
func (d *DB) ListActiveJobs() ([]models.Job, error) {
	rows, err := d.sql.Query(`
		SELECT id, kind, target, status, progress, message, error, created_by, created_at, started_at, ended_at
		FROM jobs WHERE status IN ('queued', 'running') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Job
	for rows.Next() {
		j, err := scanJobRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// FailStaleJobs marks jobs that were mid-run at a restart as failed: their
// work was cut off and cannot be resumed. Jobs that never started stay queued
// and are picked up again.
func (d *DB) FailStaleJobs() (int64, error) {
	res, err := d.sql.Exec(`
		UPDATE jobs SET status = 'failed', error = 'interrupted by console restart', payload = '', ended_at = CURRENT_TIMESTAMP
		WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeJobs keeps the job table small.
func (d *DB) PurgeJobs(keep int) error {
	if keep <= 0 {
		keep = 200
	}
	_, err := d.sql.Exec(`
		DELETE FROM jobs WHERE id NOT IN (
			SELECT id FROM jobs ORDER BY created_at DESC LIMIT ?
		)`, keep)
	return err
}

func scanJob(row *sql.Row) (*models.Job, error) {
	j, err := scanJobRows(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

func scanJobRows(row rowScanner) (*models.Job, error) {
	var j models.Job
	var created, started, ended any
	if err := row.Scan(&j.ID, &j.Kind, &j.Target, &j.Status, &j.Progress, &j.Message, &j.Error,
		&j.CreatedBy, &created, &started, &ended); err != nil {
		return nil, err
	}
	j.CreatedAt = parseTime(created)
	j.StartedAt = nullableTime(started)
	j.EndedAt = nullableTime(ended)
	return &j, nil
}
