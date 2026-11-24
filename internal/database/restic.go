package database

import (
	"database/sql"
	"errors"
	"time"

	"github.com/peaceful/cloud-console/internal/models"
)

// CreateBackupDestination stores a new Restic destination with its secrets
// sealed at rest.
func (d *DB) CreateBackupDestination(dest *models.BackupDestination) (int64, error) {
	sealedPass, err := d.sealSecret(dest.RepoPassword)
	if err != nil {
		return 0, err
	}
	sealedS3, err := d.sealSecret(dest.S3SecretKey)
	if err != nil {
		return 0, err
	}
	sealedSSH, err := d.sealSecret(dest.SSHPrivateKey)
	if err != nil {
		return 0, err
	}

	res, err := d.sql.Exec(`
		INSERT INTO backup_destinations (
			user_id, name, kind, repo_url, repo_password,
			s3_access_key, s3_secret_key, s3_region,
			ssh_private_key, ssh_public_key, ssh_known_hosts,
			keep_last, keep_daily, keep_weekly
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dest.UserID, dest.Name, dest.Kind, dest.RepoURL, sealedPass,
		dest.S3AccessKey, sealedS3, dest.S3Region,
		sealedSSH, dest.SSHPublicKey, dest.SSHKnownHosts,
		dest.KeepLast, dest.KeepDaily, dest.KeepWeekly,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err == nil {
		dest.ID = id
	}
	return id, err
}

// GetBackupDestination loads one destination and unseals its secrets.
func (d *DB) GetBackupDestination(id int64) (*models.BackupDestination, error) {
	row := d.sql.QueryRow(`
		SELECT id, user_id, name, kind, repo_url, repo_password,
		       s3_access_key, s3_secret_key, s3_region,
		       ssh_private_key, ssh_public_key, ssh_known_hosts,
		       keep_last, keep_daily, keep_weekly,
		       last_checked_at, last_check_status, created_at
		FROM backup_destinations WHERE id = ?`, id)
	return d.scanBackupDestination(row)
}

// ListBackupDestinationsScoped returns destinations visible to the scope.
// Admins see all destinations; tenants see their own.
func (d *DB) ListBackupDestinationsScoped(scope models.Scope) ([]models.BackupDestination, error) {
	var rows *sql.Rows
	var err error
	if scope.All() {
		rows, err = d.sql.Query(`
			SELECT id, user_id, name, kind, repo_url, repo_password,
			       s3_access_key, s3_secret_key, s3_region,
			       ssh_private_key, ssh_public_key, ssh_known_hosts,
			       keep_last, keep_daily, keep_weekly,
			       last_checked_at, last_check_status, created_at
			FROM backup_destinations ORDER BY created_at DESC`)
	} else {
		rows, err = d.sql.Query(`
			SELECT id, user_id, name, kind, repo_url, repo_password,
			       s3_access_key, s3_secret_key, s3_region,
			       ssh_private_key, ssh_public_key, ssh_known_hosts,
			       keep_last, keep_daily, keep_weekly,
			       last_checked_at, last_check_status, created_at
			FROM backup_destinations WHERE user_id = ? ORDER BY created_at DESC`, scope.UserID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.BackupDestination
	for rows.Next() {
		dest, err := d.scanBackupDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *dest)
	}
	return out, rows.Err()
}

// DeleteBackupDestination removes a destination and its snapshot metadata.
func (d *DB) DeleteBackupDestination(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM backup_destinations WHERE id = ?`, id)
	return err
}

// UpdateBackupDestinationCheck records the latest restic check result.
func (d *DB) UpdateBackupDestinationCheck(id int64, status string, checkedAt time.Time) error {
	_, err := d.sql.Exec(`
		UPDATE backup_destinations
		SET last_check_status = ?, last_checked_at = ?
		WHERE id = ?`, status, checkedAt, id)
	return err
}

func (d *DB) scanBackupDestination(s rowScanner) (*models.BackupDestination, error) {
	var dest models.BackupDestination
	var sealedPass, sealedS3, sealedSSH string
	var checkedAt sql.NullTime
	err := s.Scan(
		&dest.ID, &dest.UserID, &dest.Name, &dest.Kind, &dest.RepoURL, &sealedPass,
		&dest.S3AccessKey, &sealedS3, &dest.S3Region,
		&sealedSSH, &dest.SSHPublicKey, &dest.SSHKnownHosts,
		&dest.KeepLast, &dest.KeepDaily, &dest.KeepWeekly,
		&checkedAt, &dest.LastCheckStatus, &dest.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if checkedAt.Valid {
		t := checkedAt.Time
		dest.LastCheckedAt = &t
	}
	if dest.RepoPassword, err = d.openSecret(sealedPass); err != nil {
		return nil, err
	}
	if dest.S3SecretKey, err = d.openSecret(sealedS3); err != nil {
		return nil, err
	}
	if dest.SSHPrivateKey, err = d.openSecret(sealedSSH); err != nil {
		return nil, err
	}
	return &dest, nil
}

// SaveResticSnapshot records a Restic snapshot in SQLite.
func (d *DB) SaveResticSnapshot(snap *models.ResticSnapshot) (int64, error) {
	res, err := d.sql.Exec(`
		INSERT INTO restic_snapshots (
			destination_id, instance_name, snapshot_id, backup_name, repo_prefix,
			size_bytes, data_added_bytes, duration_sec, status, error, created_by
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.DestinationID, snap.InstanceName, snap.SnapshotID, snap.BackupName, snap.RepoPrefix,
		snap.SizeBytes, snap.DataAddedBytes, snap.DurationSec, snap.Status, snap.Error, snap.CreatedBy,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err == nil {
		snap.ID = id
	}
	return id, err
}

// GetResticSnapshot loads a single Restic snapshot record.
func (d *DB) GetResticSnapshot(id int64) (*models.ResticSnapshot, error) {
	row := d.sql.QueryRow(`
		SELECT rs.id, rs.destination_id, bd.name, bd.kind, rs.instance_name, rs.snapshot_id,
		       rs.backup_name, rs.repo_prefix, rs.size_bytes, rs.data_added_bytes, rs.duration_sec,
		       rs.status, rs.error, rs.created_by, rs.created_at
		FROM restic_snapshots rs
		JOIN backup_destinations bd ON bd.id = rs.destination_id
		WHERE rs.id = ?`, id)
	var s models.ResticSnapshot
	err := row.Scan(
		&s.ID, &s.DestinationID, &s.DestinationName, &s.DestinationKind,
		&s.InstanceName, &s.SnapshotID, &s.BackupName, &s.RepoPrefix,
		&s.SizeBytes, &s.DataAddedBytes, &s.DurationSec,
		&s.Status, &s.Error, &s.CreatedBy, &s.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// ListResticSnapshotsForInstance lists Restic snapshots for one instance.
func (d *DB) ListResticSnapshotsForInstance(instanceName string) ([]models.ResticSnapshot, error) {
	return d.queryResticSnapshots(`
		SELECT rs.id, rs.destination_id, bd.name, bd.kind, rs.instance_name, rs.snapshot_id,
		       rs.backup_name, rs.repo_prefix, rs.size_bytes, rs.data_added_bytes, rs.duration_sec,
		       rs.status, rs.error, rs.created_by, rs.created_at
		FROM restic_snapshots rs
		JOIN backup_destinations bd ON bd.id = rs.destination_id
		WHERE rs.instance_name = ?
		ORDER BY rs.created_at DESC`, instanceName)
}

// ListAllResticSnapshotsScoped lists Restic snapshots visible to the scope.
func (d *DB) ListAllResticSnapshotsScoped(scope models.Scope) ([]models.ResticSnapshot, error) {
	if scope.All() {
		return d.queryResticSnapshots(`
			SELECT rs.id, rs.destination_id, bd.name, bd.kind, rs.instance_name, rs.snapshot_id,
			       rs.backup_name, rs.repo_prefix, rs.size_bytes, rs.data_added_bytes, rs.duration_sec,
			       rs.status, rs.error, rs.created_by, rs.created_at
			FROM restic_snapshots rs
			JOIN backup_destinations bd ON bd.id = rs.destination_id
			ORDER BY rs.created_at DESC`)
	}
	return d.queryResticSnapshots(`
		SELECT rs.id, rs.destination_id, bd.name, bd.kind, rs.instance_name, rs.snapshot_id,
		       rs.backup_name, rs.repo_prefix, rs.size_bytes, rs.data_added_bytes, rs.duration_sec,
		       rs.status, rs.error, rs.created_by, rs.created_at
		FROM restic_snapshots rs
		JOIN backup_destinations bd ON bd.id = rs.destination_id
		JOIN instances i ON i.name = rs.instance_name
		WHERE i.owner_id = ?
		ORDER BY rs.created_at DESC`, scope.UserID)
}

// DeleteResticSnapshot removes one snapshot metadata row.
func (d *DB) DeleteResticSnapshot(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM restic_snapshots WHERE id = ?`, id)
	return err
}

// SyncResticSnapshots removes local metadata rows for a (destination, instance)
// whose snapshot_id is no longer present in the given keep set (used after prune).
func (d *DB) SyncResticSnapshots(destinationID int64, instanceName string, keepSnapshotIDs map[string]bool) error {
	snaps, err := d.ListResticSnapshotsForInstance(instanceName)
	if err != nil {
		return err
	}
	for _, s := range snaps {
		if s.DestinationID != destinationID || s.Status != "complete" {
			continue
		}
		if !keepSnapshotIDs[s.SnapshotID] && (len(s.SnapshotID) < 8 || !keepSnapshotIDs[s.SnapshotID[:8]]) {
			if err := d.DeleteResticSnapshot(s.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// DistinctInstancesForDestination returns instance names that have snapshots
// in the given destination.
func (d *DB) DistinctInstancesForDestination(destinationID int64) ([]string, error) {
	rows, err := d.sql.Query(`
		SELECT DISTINCT instance_name FROM restic_snapshots WHERE destination_id = ? ORDER BY instance_name`, destinationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (d *DB) queryResticSnapshots(query string, args ...any) ([]models.ResticSnapshot, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ResticSnapshot
	for rows.Next() {
		var s models.ResticSnapshot
		if err := rows.Scan(
			&s.ID, &s.DestinationID, &s.DestinationName, &s.DestinationKind,
			&s.InstanceName, &s.SnapshotID, &s.BackupName, &s.RepoPrefix,
			&s.SizeBytes, &s.DataAddedBytes, &s.DurationSec,
			&s.Status, &s.Error, &s.CreatedBy, &s.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
