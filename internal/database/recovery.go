package database

// ReplaceRecoveryCodes swaps a user's recovery codes for a new set. Older
// codes stop working the moment the new set exists.
func (d *DB) ReplaceRecoveryCodes(userID int64, hashes []string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(`INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)`, userID, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConsumeRecoveryCode marks a code used and reports whether it was valid and
// unused. The single UPDATE makes redeeming atomic: of two simultaneous
// attempts with the same code, only one sees a changed row.
func (d *DB) ConsumeRecoveryCode(userID int64, hash string) (bool, error) {
	res, err := d.sql.Exec(
		`UPDATE recovery_codes SET used_at = CURRENT_TIMESTAMP
		 WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`, userID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CountRecoveryCodes returns how many unused codes a user has left.
func (d *DB) CountRecoveryCodes(userID int64) (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

// DeleteRecoveryCodes removes all of a user's codes.
func (d *DB) DeleteRecoveryCodes(userID int64) error {
	_, err := d.sql.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, userID)
	return err
}
