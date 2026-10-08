package database

import (
	"database/sql"
	"errors"

	"github.com/peaceful/cloud-console/internal/models"
)

// ListProviders returns all SSO providers by name.
func (d *DB) ListProviders() ([]models.SSOProvider, error) {
	rows, err := d.sql.Query(
		`SELECT id, name, issuer, client_id, client_secret, button_label, default_role, require_mfa, enabled, created_at
		 FROM sso_providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.SSOProvider
	for rows.Next() {
		var p models.SSOProvider
		var enabled, requireMFA int64
		var created any
		if err := rows.Scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &p.ClientSecret,
			&p.ButtonLabel, &p.DefaultRole, &requireMFA, &enabled, &created); err != nil {
			return nil, err
		}
		p.RequireMFA = requireMFA == 1
		p.Enabled = enabled == 1
		p.CreatedAt = parseTime(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProvider loads one SSO provider.
func (d *DB) GetProvider(id int64) (*models.SSOProvider, error) {
	row := d.sql.QueryRow(
		`SELECT id, name, issuer, client_id, client_secret, button_label, default_role, require_mfa, enabled, created_at
		 FROM sso_providers WHERE id = ?`, id)

	var p models.SSOProvider
	var enabled, requireMFA int64
	var created any
	if err := row.Scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &p.ClientSecret,
		&p.ButtonLabel, &p.DefaultRole, &requireMFA, &enabled, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.RequireMFA = requireMFA == 1
	p.Enabled = enabled == 1
	p.CreatedAt = parseTime(created)
	return &p, nil
}

// SaveProvider inserts or updates an SSO provider.
func (d *DB) SaveProvider(p *models.SSOProvider) error {
	requireMFA := 0
	if p.RequireMFA {
		requireMFA = 1
	}
	enabled := 0
	if p.Enabled {
		enabled = 1
	}

	if p.ID == 0 {
		res, err := d.sql.Exec(
			`INSERT INTO sso_providers (name, issuer, client_id, client_secret, button_label, default_role, require_mfa, enabled)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			p.Name, p.Issuer, p.ClientID, p.ClientSecret, p.ButtonLabel, p.DefaultRole, requireMFA, enabled)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		p.ID = id
		return nil
	}

	_, err := d.sql.Exec(
		`UPDATE sso_providers SET name = ?, issuer = ?, client_id = ?, client_secret = ?,
		 button_label = ?, default_role = ?, require_mfa = ?, enabled = ? WHERE id = ?`,
		p.Name, p.Issuer, p.ClientID, p.ClientSecret, p.ButtonLabel, p.DefaultRole, requireMFA, enabled, p.ID)
	return err
}

// DeleteProvider removes a provider and its identity links.
func (d *DB) DeleteProvider(id int64) error {
	if _, err := d.sql.Exec(`DELETE FROM user_identities WHERE provider_id = ?`, id); err != nil {
		return err
	}
	_, err := d.sql.Exec(`DELETE FROM sso_providers WHERE id = ?`, id)
	return err
}

// CountIdentitiesForProvider counts linked accounts on a provider.
func (d *DB) CountIdentitiesForProvider(providerID int64) (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM user_identities WHERE provider_id = ?`, providerID).Scan(&n)
	return n, err
}

// LinkIdentity records that a console user owns a subject at a provider.
func (d *DB) LinkIdentity(userID, providerID int64, subject string) error {
	_, err := d.sql.Exec(
		`INSERT INTO user_identities (user_id, provider_id, subject) VALUES (?, ?, ?)
		 ON CONFLICT (provider_id, subject) DO UPDATE SET user_id = excluded.user_id`,
		userID, providerID, subject)
	return err
}

// FindIdentityUser resolves a provider subject to its console user id.
func (d *DB) FindIdentityUser(providerID int64, subject string) (int64, error) {
	var userID int64
	err := d.sql.QueryRow(
		`SELECT user_id FROM user_identities WHERE provider_id = ? AND subject = ?`,
		providerID, subject).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return userID, nil
}

// ListIdentities returns every linked identity, newest links last.
func (d *DB) ListIdentities() ([]models.UserIdentity, error) {
	rows, err := d.sql.Query(
		`SELECT user_id, provider_id, subject, created_at FROM user_identities ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.UserIdentity
	for rows.Next() {
		var ident models.UserIdentity
		var created any
		if err := rows.Scan(&ident.UserID, &ident.ProviderID, &ident.Subject, &created); err != nil {
			return nil, err
		}
		ident.CreatedAt = parseTime(created)
		out = append(out, ident)
	}
	return out, rows.Err()
}

// SetAuthMethod changes which sign-in paths a user may use.
func (d *DB) SetAuthMethod(userID int64, method string) error {
	_, err := d.sql.Exec(`UPDATE users SET auth_method = ? WHERE id = ?`, method, userID)
	return err
}
