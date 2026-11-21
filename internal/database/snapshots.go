package database

import (
	"database/sql"
	"errors"

	"github.com/peaceful/cloud-console/internal/models"
)

// SaveSnapshotMeta records console bookkeeping for an Incus snapshot.
func (d *DB) SaveSnapshotMeta(m *models.SnapshotMeta) error {
	_, err := d.sql.Exec(`
		INSERT INTO snapshots (instance_name, name, note, created_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(instance_name, name) DO UPDATE SET
			note = excluded.note,
			created_by = excluded.created_by`,
		m.InstanceName, m.Name, m.Note, m.CreatedBy,
	)
	return err
}

// GetSnapshotMeta loads console metadata for one snapshot.
func (d *DB) GetSnapshotMeta(instanceName, name string) (*models.SnapshotMeta, error) {
	row := d.sql.QueryRow(
		`SELECT id, instance_name, name, note, created_by, created_at FROM snapshots WHERE instance_name = ? AND name = ?`,
		instanceName, name)

	var m models.SnapshotMeta
	var created any
	err := row.Scan(&m.ID, &m.InstanceName, &m.Name, &m.Note, &m.CreatedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = parseTime(created)
	return &m, nil
}

// ListSnapshotMetaForInstance returns console metadata for an instance's snapshots.
func (d *DB) ListSnapshotMetaForInstance(instanceName string) (map[string]models.SnapshotMeta, error) {
	rows, err := d.sql.Query(
		`SELECT id, instance_name, name, note, created_by, created_at FROM snapshots WHERE instance_name = ?`,
		instanceName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]models.SnapshotMeta{}
	for rows.Next() {
		var m models.SnapshotMeta
		var created any
		if err := rows.Scan(&m.ID, &m.InstanceName, &m.Name, &m.Note, &m.CreatedBy, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		out[m.Name] = m
	}
	return out, rows.Err()
}

// ListAllSnapshotMeta returns every snapshot record.
func (d *DB) ListAllSnapshotMeta() ([]models.SnapshotMeta, error) {
	return d.querySnapshotMeta(
		`SELECT id, instance_name, name, note, created_by, created_at FROM snapshots ORDER BY created_at DESC`)
}

// ListAllSnapshotMetaScoped returns the snapshot records the scope may see.
func (d *DB) ListAllSnapshotMetaScoped(scope models.Scope) ([]models.SnapshotMeta, error) {
	if scope.All() {
		return d.ListAllSnapshotMeta()
	}
	return d.querySnapshotMeta(`
		SELECT s.id, s.instance_name, s.name, s.note, s.created_by, s.created_at
		FROM snapshots s
		JOIN instances i ON i.name = s.instance_name
		WHERE i.owner_id = ?
		ORDER BY s.created_at DESC`, scope.UserID)
}

func (d *DB) querySnapshotMeta(query string, args ...any) ([]models.SnapshotMeta, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.SnapshotMeta
	for rows.Next() {
		var m models.SnapshotMeta
		var created any
		if err := rows.Scan(&m.ID, &m.InstanceName, &m.Name, &m.Note, &m.CreatedBy, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteSnapshotMeta removes console metadata for a snapshot.
func (d *DB) DeleteSnapshotMeta(instanceName, name string) error {
	_, err := d.sql.Exec(`DELETE FROM snapshots WHERE instance_name = ? AND name = ?`, instanceName, name)
	return err
}

// DeleteSnapshotsForInstance removes all snapshot metadata for an instance.
func (d *DB) DeleteSnapshotsForInstance(instanceName string) error {
	_, err := d.sql.Exec(`DELETE FROM snapshots WHERE instance_name = ?`, instanceName)
	return err
}

// RenameSnapshots moves snapshot metadata when an instance is renamed.
func (d *DB) RenameSnapshots(oldName, newName string) error {
	_, err := d.sql.Exec(`UPDATE snapshots SET instance_name = ? WHERE instance_name = ?`, newName, oldName)
	return err
}

// CountSnapshots returns the number of tracked snapshots.
func (d *DB) CountSnapshots() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM snapshots`).Scan(&n)
	return n, err
}

// CountSnapshotsScoped counts the tracked snapshots the scope may see.
func (d *DB) CountSnapshotsScoped(scope models.Scope) (int, error) {
	if scope.All() {
		return d.CountSnapshots()
	}
	var n int
	err := d.sql.QueryRow(`
		SELECT COUNT(1) FROM snapshots s
		JOIN instances i ON i.name = s.instance_name
		WHERE i.owner_id = ?`, scope.UserID).Scan(&n)
	return n, err
}
