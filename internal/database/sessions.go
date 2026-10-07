package database

import (
	"database/sql"
	"errors"
	"time"

	"github.com/peaceful/cloud-console/internal/models"
)

// CreateSession stores a hashed session token.
func (d *DB) CreateSession(tokenHash string, userID int64, expires time.Time, ip, userAgent string) error {
	_, err := d.sql.Exec(
		`INSERT INTO sessions (token_hash, user_id, expires_at, ip, user_agent) VALUES (?, ?, ?, ?, ?)`,
		tokenHash, userID, expires.UTC().Format(timeLayout), ip, userAgent,
	)
	return err
}

// SessionUser resolves a session token hash to its user, if the session is
// still valid.
func (d *DB) SessionUser(tokenHash string) (*models.User, error) {
	row := d.sql.QueryRow(`
		SELECT u.id, u.username, u.password_hash, u.role, u.created_at, u.last_login_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, time.Now().UTC().Format(timeLayout))

	var u models.User
	var created any
	var last any
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = parseTime(created)
	u.LastLoginAt = nullableTime(last)
	return &u, nil
}

// DeleteSession signs a single session out.
func (d *DB) DeleteSession(tokenHash string) error {
	_, err := d.sql.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessions signs every session of a user out.
func (d *DB) DeleteUserSessions(userID int64) error {
	_, err := d.sql.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeExpiredSessions removes stale sessions and returns how many were removed.
func (d *DB) PurgeExpiredSessions() (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().UTC().Format(timeLayout))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountSessions returns the number of active sessions.
func (d *DB) CountSessions() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM sessions WHERE expires_at > ?`,
		time.Now().UTC().Format(timeLayout)).Scan(&n)
	return n, err
}
