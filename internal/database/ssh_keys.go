package database

import (
	"database/sql"
	"errors"

	"github.com/peaceful/cloud-console/internal/models"
)

// Every method in this file is scoped by owner id. There is deliberately no
// unscoped variant: SSH keys are personal credentials, and even an administrator
// manages only their own keys. A key is resolved for an instance by the
// instance's owner (see GetSSHKeyForUser in the create handler), never by the
// actor, so an admin creating on behalf of a tenant still cannot cross tenants.

// ListSSHKeysForUser returns a tenant's keys, oldest first.
func (d *DB) ListSSHKeysForUser(userID int64) ([]models.SSHKey, error) {
	rows, err := d.sql.Query(`
		SELECT id, COALESCE(owner_id, 0), name, public_key, fingerprint, created_at
		FROM ssh_keys WHERE owner_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []models.SSHKey{}
	for rows.Next() {
		var k models.SSHKey
		var created any
		if err := rows.Scan(&k.ID, &k.OwnerID, &k.Name, &k.PublicKey, &k.Fingerprint, &created); err != nil {
			return nil, err
		}
		k.CreatedAt = parseTime(created)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// GetSSHKeyForUser loads one key, but only when it belongs to userID.
func (d *DB) GetSSHKeyForUser(userID, id int64) (*models.SSHKey, error) {
	row := d.sql.QueryRow(`
		SELECT id, COALESCE(owner_id, 0), name, public_key, fingerprint, created_at
		FROM ssh_keys WHERE owner_id = ? AND id = ?`, userID, id)

	var k models.SSHKey
	var created any
	err := row.Scan(&k.ID, &k.OwnerID, &k.Name, &k.PublicKey, &k.Fingerprint, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.CreatedAt = parseTime(created)
	return &k, nil
}

// CreateSSHKey stores a new public key for its owner.
func (d *DB) CreateSSHKey(k *models.SSHKey) error {
	res, err := d.sql.Exec(`
		INSERT INTO ssh_keys (owner_id, name, public_key, fingerprint)
		VALUES (?, ?, ?, ?)`, k.OwnerID, k.Name, k.PublicKey, k.Fingerprint)
	if err != nil {
		return err
	}
	k.ID, err = res.LastInsertId()
	return err
}

// DeleteSSHKeyForUser removes a key only when it belongs to userID.
func (d *DB) DeleteSSHKeyForUser(userID, id int64) error {
	res, err := d.sql.Exec(`DELETE FROM ssh_keys WHERE owner_id = ? AND id = ?`, userID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RenameSSHKeyForUser renames a key only when it belongs to userID.
func (d *DB) RenameSSHKeyForUser(userID, id int64, name string) error {
	res, err := d.sql.Exec(`UPDATE ssh_keys SET name = ? WHERE owner_id = ? AND id = ?`, name, userID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
