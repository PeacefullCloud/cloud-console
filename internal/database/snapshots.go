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

// ListAllSnapshotMeta returns every snapshot record grouped by instance.
func (d *DB) ListAllSnapshotMeta() ([]models.SnapshotMeta, error) {
	rows, err := d.sql.Query(
		`SELECT id, instance_name, name, note, created_by, created_at FROM snapshots ORDER BY created_at DESC`)
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
