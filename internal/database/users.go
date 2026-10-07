package database

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/peaceful/cloud-console/internal/models"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// CountUsers returns the number of console users.
func (d *DB) CountUsers() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(1) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a user and returns it.
func (d *DB) CreateUser(username, passwordHash, role string) (*models.User, error) {
	res, err := d.sql.Exec(
		`INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)`,
		username, passwordHash, role,
	)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.GetUser(id)
}

// GetUser loads a user by id.
func (d *DB) GetUser(id int64) (*models.User, error) {
	return d.scanUser(d.sql.QueryRow(
		`SELECT id, username, password_hash, role, created_at, last_login_at FROM users WHERE id = ?`, id))
}

// GetUserByUsername loads a user by username.
func (d *DB) GetUserByUsername(username string) (*models.User, error) {
	return d.scanUser(d.sql.QueryRow(
		`SELECT id, username, password_hash, role, created_at, last_login_at FROM users WHERE username = ?`, username))
}

// ListUsers returns all users, newest first.
func (d *DB) ListUsers() ([]models.User, error) {
	rows, err := d.sql.Query(
		`SELECT id, username, password_hash, role, created_at, last_login_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.User
	for rows.Next() {
		var u models.User
		var created any
		var last any
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created, &last); err != nil {
			return nil, err
		}
		u.CreatedAt = parseTime(created)
		u.LastLoginAt = nullableTime(last)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (d *DB) scanUser(row *sql.Row) (*models.User, error) {
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

// TouchLastLogin records a successful sign-in.
func (d *DB) TouchLastLogin(id int64) error {
	_, err := d.sql.Exec(`UPDATE users SET last_login_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}

// UpdatePassword changes a user's password hash.
func (d *DB) UpdatePassword(id int64, hash string) error {
	_, err := d.sql.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

// DeleteUser removes a user and their sessions.
func (d *DB) DeleteUser(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}
