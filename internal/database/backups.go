package database

import (
	"database/sql"
	"errors"

	"github.com/peaceful/cloud-console/internal/models"
)

// SaveBackup inserts or updates a backup record.
func (d *DB) SaveBackup(b *models.Backup) error {
	_, err := d.sql.Exec(`
		INSERT INTO backups (instance_name, name, target, s3_key, size_bytes, status, error, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(instance_name, name) DO UPDATE SET
			target     = excluded.target,
			s3_key     = excluded.s3_key,
			size_bytes = excluded.size_bytes,
			status     = excluded.status,
			error      = excluded.error`,
		b.InstanceName, b.Name, b.Target, b.S3Key, b.SizeBytes, b.Status, b.Error, b.CreatedBy,
	)
	return err
}

// UpdateBackupStatus updates the lifecycle state of a backup.
func (d *DB) UpdateBackupStatus(instanceName, name, status, errMsg string, size int64, s3Key string) error {
	_, err := d.sql.Exec(`
		UPDATE backups
		SET status = ?, error = ?, size_bytes = ?, s3_key = ?
		WHERE instance_name = ? AND name = ?`,
		status, errMsg, size, s3Key, instanceName, name)
	return err
}

// GetBackup loads one backup record.
func (d *DB) GetBackup(instanceName, name string) (*models.Backup, error) {
	row := d.sql.QueryRow(`
		SELECT id, instance_name, name, target, s3_key, size_bytes, status, error, created_by, created_at
		FROM backups WHERE instance_name = ? AND name = ?`, instanceName, name)
	return scanBackup(row)
}

// ListBackupsForInstance returns the backups recorded for an instance.
func (d *DB) ListBackupsForInstance(instanceName string) ([]models.Backup, error) {
	return d.queryBackups(`
		SELECT id, instance_name, name, target, s3_key, size_bytes, status, error, created_by, created_at
		FROM backups WHERE instance_name = ? ORDER BY created_at DESC`, instanceName)
}

// ListAllBackups returns every backup record.
func (d *DB) ListAllBackups() ([]models.Backup, error) {
	return d.queryBackups(`
		SELECT id, instance_name, name, target, s3_key, size_bytes, status, error, created_by, created_at
		FROM backups ORDER BY created_at DESC`)
}

// DeleteBackup removes a backup record.
func (d *DB) DeleteBackup(instanceName, name string) error {
	_, err := d.sql.Exec(`DELETE FROM backups WHERE instance_name = ? AND name = ?`, instanceName, name)
	return err
}

// DeleteBackupsForInstance removes all backup records for an instance.
func (d *DB) DeleteBackupsForInstance(instanceName string) error {
	_, err := d.sql.Exec(`DELETE FROM backups WHERE instance_name = ?`, instanceName)
	return err
}

// RenameBackups moves backup records when an instance is renamed.
func (d *DB) RenameBackups(oldName, newName string) error {
	_, err := d.sql.Exec(`UPDATE backups SET instance_name = ? WHERE instance_name = ?`, newName, oldName)
	return err
}

// BackupTotals returns the count of backups and their total size.
func (d *DB) BackupTotals() (int, int64, error) {
	var count int
	var size sql.NullInt64
	err := d.sql.QueryRow(`SELECT COUNT(1), SUM(size_bytes) FROM backups WHERE status = 'complete'`).Scan(&count, &size)
	return count, size.Int64, err
}

func (d *DB) queryBackups(query string, args ...any) ([]models.Backup, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Backup
	for rows.Next() {
		b, err := scanBackupRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func scanBackup(row *sql.Row) (*models.Backup, error) {
	b, err := scanBackupRows(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func scanBackupRows(row rowScanner) (*models.Backup, error) {
	var b models.Backup
	var created any
	if err := row.Scan(&b.ID, &b.InstanceName, &b.Name, &b.Target, &b.S3Key, &b.SizeBytes,
		&b.Status, &b.Error, &b.CreatedBy, &created); err != nil {
		return nil, err
	}
	b.CreatedAt = parseTime(created)
	return &b, nil
}
