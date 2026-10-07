package database

import (
	"github.com/peaceful/cloud-console/internal/models"
)

// LogActivity appends an audit-log entry.
func (d *DB) LogActivity(username, action, target, detail, status string) error {
	_, err := d.sql.Exec(
		`INSERT INTO activity (username, action, target, detail, status) VALUES (?, ?, ?, ?, ?)`,
		username, action, target, detail, status,
	)
	return err
}

// ListActivity returns recent activity, newest first.
func (d *DB) ListActivity(limit, offset int) ([]models.Activity, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.sql.Query(`
		SELECT id, ts, username, action, target, detail, status
		FROM activity ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Activity
	for rows.Next() {
		var a models.Activity
		var ts any
		if err := rows.Scan(&a.ID, &ts, &a.Username, &a.Action, &a.Target, &a.Detail, &a.Status); err != nil {
			return nil, err
		}
		a.TS = parseTime(ts)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListActivityForInstance returns activity entries mentioning an instance.
func (d *DB) ListActivityForInstance(instanceName string, limit int) ([]models.Activity, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.sql.Query(`
		SELECT id, ts, username, action, target, detail, status
		FROM activity WHERE target = ? ORDER BY id DESC LIMIT ?`, instanceName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Activity
	for rows.Next() {
		var a models.Activity
		var ts any
		if err := rows.Scan(&a.ID, &ts, &a.Username, &a.Action, &a.Target, &a.Detail, &a.Status); err != nil {
			return nil, err
		}
		a.TS = parseTime(ts)
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountActivity returns the total number of activity rows.
func (d *DB) CountActivity() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM activity`).Scan(&n)
	return n, err
}
