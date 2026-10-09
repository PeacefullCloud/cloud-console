// Package database is the SQLite data layer for the console.
//
// It stores only console metadata: users, sessions, instance bookkeeping,
// domains, snapshot/backup records, activity and sampled metrics.
package database

import (
	"database/sql"
	"fmt"
	"github.com/peaceful/cloud-console/internal/secrets"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo

	"github.com/peaceful/cloud-console/migrations"
)

// DB wraps the SQLite connection pool.
type DB struct {
	sql *sql.DB
	log *slog.Logger

	// sealer protects TOTP seeds at rest. Nil leaves them in plaintext,
	// which only tests that do not care about it rely on.
	sealer *secrets.Sealer
}

// Open opens (creating if needed) the SQLite database at path and applies any
// pending migrations.
func Open(path string, log *slog.Logger) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)

	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite handles one writer; a small pool avoids lock churn.
	raw.SetMaxOpenConns(4)
	raw.SetMaxIdleConns(4)
	raw.SetConnMaxLifetime(time.Hour)

	if err := raw.Ping(); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	db := &DB{sql: raw, log: log}
	if err := db.migrate(); err != nil {
		_ = raw.Close()
		return nil, err
	}

	return db, nil
}

// Close releases the connection pool.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the underlying pool for packages that need custom queries.
func (d *DB) SQL() *sql.DB { return d.sql }

// migrate applies every embedded migration that has not run yet.
func (d *DB) migrate() error {
	if _, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists int
		err := d.sql.QueryRow(`SELECT COUNT(1) FROM schema_migrations WHERE name = ?`, name).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists > 0 {
			continue
		}

		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := d.sql.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}

		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}

		d.log.Info("applied migration", "name", name)
	}

	return nil
}

// timeLayout is the format SQLite stores DATETIME values created by
// CURRENT_TIMESTAMP.
const timeLayout = "2006-01-02 15:04:05"

// parseTime converts a SQLite timestamp column into a time.Time.
func parseTime(v any) time.Time {
	switch t := v.(type) {
	case nil:
		return time.Time{}
	case time.Time:
		return t
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, timeLayout, "2006-01-02 15:04:05.999999999-07:00"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed
			}
		}
	case []byte:
		return parseTime(string(t))
	}
	return time.Time{}
}

func nullableTime(v any) *time.Time {
	t := parseTime(v)
	if t.IsZero() {
		return nil
	}
	return &t
}
