package database

import (
	"github.com/peaceful/cloud-console/internal/models"
)

const activityColumns = `id, ts, username, action, target, detail, status, COALESCE(owner_id, 0)`

// LogActivity appends an audit-log entry. ownerID is the tenant the entry is
// about; 0 means administrators only.
func (d *DB) LogActivity(ownerID int64, username, action, target, detail, status string) error {
	_, err := d.sql.Exec(
		`INSERT INTO activity (username, action, target, detail, status, owner_id) VALUES (?, ?, ?, ?, ?, ?)`,
		username, action, target, detail, status, nullableID(ownerID),
	)
	return err
}

// ListActivity returns recent activity, newest first.
func (d *DB) ListActivity(limit, offset int) ([]models.Activity, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return d.queryActivity(`ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
}

// ListActivityScoped returns the recent activity the scope may see.
func (d *DB) ListActivityScoped(scope models.Scope, limit, offset int) ([]models.Activity, error) {
	if scope.All() {
		return d.ListActivity(limit, offset)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return d.queryActivity(`WHERE owner_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`, scope.UserID, limit, offset)
}

// ListActivityForInstance returns activity entries mentioning an instance.
func (d *DB) ListActivityForInstance(instanceName string, limit int) ([]models.Activity, error) {
	if limit <= 0 {
		limit = 50
	}
	return d.queryActivity(`WHERE target = ? ORDER BY id DESC LIMIT ?`, instanceName, limit)
}

// CountActivity returns the total number of activity rows.
func (d *DB) CountActivity() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM activity`).Scan(&n)
	return n, err
}

// CountActivityScoped counts the activity rows the scope may see.
func (d *DB) CountActivityScoped(scope models.Scope) (int, error) {
	if scope.All() {
		return d.CountActivity()
	}
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM activity WHERE owner_id = ?`, scope.UserID).Scan(&n)
	return n, err
}

func (d *DB) queryActivity(clause string, args ...any) ([]models.Activity, error) {
	rows, err := d.sql.Query(`SELECT `+activityColumns+` FROM activity `+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Activity
	for rows.Next() {
		var a models.Activity
		var ts any
		if err := rows.Scan(&a.ID, &ts, &a.Username, &a.Action, &a.Target, &a.Detail, &a.Status, &a.OwnerID); err != nil {
			return nil, err
		}
		a.TS = parseTime(ts)
		out = append(out, a)
	}
	return out, rows.Err()
}
