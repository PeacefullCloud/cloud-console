package database

// GetSetting returns a setting value or def when unset.
func (d *DB) GetSetting(key, def string) string {
	var value string
	err := d.sql.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		return def
	}
	return value
}

// SetSetting stores a setting value.
func (d *DB) SetSetting(key, value string) error {
	_, err := d.sql.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AllSettings returns every setting as a map.
func (d *DB) AllSettings() (map[string]string, error) {
	rows, err := d.sql.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
