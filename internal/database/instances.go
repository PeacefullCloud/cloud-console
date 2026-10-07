package database

import (
	"database/sql"
	"errors"

	"github.com/peaceful/cloud-console/internal/models"
)

// SaveInstanceMeta inserts or updates the console record for an instance.
func (d *DB) SaveInstanceMeta(m *models.InstanceMeta) error {
	_, err := d.sql.Exec(`
		INSERT INTO instances (name, image, image_label, kind, cpu, memory_mb, disk_gb, storage_pool, owner_id, primary_host, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			image        = excluded.image,
			image_label  = excluded.image_label,
			kind         = excluded.kind,
			cpu          = excluded.cpu,
			memory_mb    = excluded.memory_mb,
			disk_gb      = excluded.disk_gb,
			storage_pool = excluded.storage_pool,
			owner_id     = excluded.owner_id,
			primary_host = excluded.primary_host,
			notes        = excluded.notes,
			updated_at   = CURRENT_TIMESTAMP`,
		m.Name, m.Image, m.ImageLabel, m.Kind, m.CPU, m.MemoryMB, m.DiskGB, m.StoragePool,
		nullableID(m.OwnerID), m.PrimaryHost, m.Notes,
	)
	return err
}

// GetInstanceMeta loads the console record for one instance.
func (d *DB) GetInstanceMeta(name string) (*models.InstanceMeta, error) {
	row := d.sql.QueryRow(`
		SELECT i.id, i.name, i.image, i.image_label, i.kind, i.cpu, i.memory_mb, i.disk_gb,
		       i.storage_pool, COALESCE(i.owner_id, 0), COALESCE(u.username, ''), i.primary_host,
		       i.notes, i.created_at, i.updated_at
		FROM instances i
		LEFT JOIN users u ON u.id = i.owner_id
		WHERE i.name = ?`, name)

	var m models.InstanceMeta
	var created, updated any
	err := row.Scan(&m.ID, &m.Name, &m.Image, &m.ImageLabel, &m.Kind, &m.CPU, &m.MemoryMB, &m.DiskGB,
		&m.StoragePool, &m.OwnerID, &m.OwnerName, &m.PrimaryHost, &m.Notes, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = parseTime(created)
	m.UpdatedAt = parseTime(updated)
	return &m, nil
}

// ListInstanceMeta returns every console instance record keyed by instance name.
func (d *DB) ListInstanceMeta() (map[string]models.InstanceMeta, error) {
	rows, err := d.sql.Query(`
		SELECT i.id, i.name, i.image, i.image_label, i.kind, i.cpu, i.memory_mb, i.disk_gb,
		       i.storage_pool, COALESCE(i.owner_id, 0), COALESCE(u.username, ''), i.primary_host,
		       i.notes, i.created_at, i.updated_at
		FROM instances i
		LEFT JOIN users u ON u.id = i.owner_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]models.InstanceMeta{}
	for rows.Next() {
		var m models.InstanceMeta
		var created, updated any
		if err := rows.Scan(&m.ID, &m.Name, &m.Image, &m.ImageLabel, &m.Kind, &m.CPU, &m.MemoryMB, &m.DiskGB,
			&m.StoragePool, &m.OwnerID, &m.OwnerName, &m.PrimaryHost, &m.Notes, &created, &updated); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		m.UpdatedAt = parseTime(updated)
		out[m.Name] = m
	}
	return out, rows.Err()
}

// DeleteInstanceMeta removes the console record for an instance.
func (d *DB) DeleteInstanceMeta(name string) error {
	_, err := d.sql.Exec(`DELETE FROM instances WHERE name = ?`, name)
	return err
}

// SetPrimaryHost points an instance's primary hostname at a domain.
func (d *DB) SetPrimaryHost(name, host string) error {
	_, err := d.sql.Exec(
		`UPDATE instances SET primary_host = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`, host, name)
	return err
}

// CountInstanceMeta returns how many instances the console tracks.
func (d *DB) CountInstanceMeta() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM instances`).Scan(&n)
	return n, err
}

func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}
