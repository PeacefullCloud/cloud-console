package database

import (
	"fmt"

	"github.com/peaceful/cloud-console/internal/secrets"
)

// SetSealer turns on encryption of TOTP seeds and seals any that are still
// stored as plaintext from earlier versions.
func (d *DB) SetSealer(s *secrets.Sealer) error {
	d.sealer = s
	return d.sealLegacySecrets()
}

func (d *DB) sealSecret(plain string) (string, error) {
	if d.sealer == nil {
		return plain, nil
	}
	return d.sealer.Seal(plain)
}

func (d *DB) openSecret(stored string) (string, error) {
	if d.sealer == nil {
		if secrets.IsSealed(stored) {
			return "", fmt.Errorf("stored secret is encrypted but no key is loaded")
		}
		return stored, nil
	}
	return d.sealer.Open(stored)
}

func (d *DB) sealLegacySecrets() error {
	rows, err := d.sql.Query(`SELECT id, totp_secret FROM users WHERE totp_secret IS NOT NULL AND totp_secret <> '' AND totp_secret NOT LIKE 'enc:v1:%'`)
	if err != nil {
		return err
	}
	type pending struct {
		id     int64
		secret string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.secret); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, p := range todo {
		sealed, err := d.sealer.Seal(p.secret)
		if err != nil {
			return err
		}
		if _, err := d.sql.Exec(`UPDATE users SET totp_secret = ? WHERE id = ?`, sealed, p.id); err != nil {
			return err
		}
	}
	if len(todo) > 0 {
		d.log.Info("encrypted stored two-factor secrets", "users", len(todo))
	}
	return nil
}
