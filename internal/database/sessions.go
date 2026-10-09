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
		SELECT u.id, u.username, u.password_hash, u.role, u.auth_method, u.created_at, u.last_login_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, time.Now().UTC().Format(timeLayout))

	var u models.User
	var created any
	var last any
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.AuthMethod, &created, &last)
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

// Session is one signed-in browser of a user. ID is a short, non-secret
// handle derived from the stored token hash so it can be shown and posted
// without exposing anything usable for authentication.
type Session struct {
	ID        string
	IP        string
	UserAgent string
	CreatedAt time.Time
	ExpiresAt time.Time
	hash      string
}

// Hash returns the stored token hash, used to recognise the current session.
func (s Session) Hash() string { return s.hash }

// sessionHandleLen is the number of token-hash characters used as a handle.
const sessionHandleLen = 16

// SessionHandle returns the public handle for a stored token hash.
func SessionHandle(tokenHash string) string {
	if len(tokenHash) > sessionHandleLen {
		return tokenHash[:sessionHandleLen]
	}
	return tokenHash
}

// ListUserSessions returns the live sessions of a user, newest first.
func (d *DB) ListUserSessions(userID int64) ([]Session, error) {
	rows, err := d.sql.Query(`
		SELECT token_hash, COALESCE(ip, ''), COALESCE(user_agent, ''), created_at, expires_at
		FROM sessions WHERE user_id = ? AND expires_at > ?
		ORDER BY created_at DESC, rowid DESC`,
		userID, time.Now().UTC().Format(timeLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var s Session
		var created, expires any
		if err := rows.Scan(&s.hash, &s.IP, &s.UserAgent, &created, &expires); err != nil {
			return nil, err
		}
		s.ID = SessionHandle(s.hash)
		s.CreatedAt = parseTime(created)
		s.ExpiresAt = parseTime(expires)
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteUserSessionByHandle revokes one session, but only if it belongs to
// the given user, so a handle cannot be used against someone else's session.
func (d *DB) DeleteUserSessionByHandle(userID int64, handle string) (bool, error) {
	if len(handle) < sessionHandleLen {
		return false, nil
	}
	res, err := d.sql.Exec(
		`DELETE FROM sessions WHERE user_id = ? AND substr(token_hash, 1, ?) = ?`,
		userID, sessionHandleLen, handle)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteOtherUserSessions signs a user out everywhere except one session.
func (d *DB) DeleteOtherUserSessions(userID int64, keepHash string) (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepHash)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
