// Package activity writes the console's audit log.
//
// Every mutating action a user takes is recorded so the Instances → History tab
// and the global Activity page can show what happened, who did it and whether it
// worked.
package activity

import (
	"log/slog"

	"github.com/peaceful/cloud-console/internal/database"
)

// Logger records activity entries.
type Logger struct {
	db  *database.DB
	log *slog.Logger
}

// New creates an activity logger.
func New(db *database.DB, log *slog.Logger) *Logger {
	return &Logger{db: db, log: log}
}

// Record writes an entry. A non-nil err marks the entry as failed.
func (l *Logger) Record(username, action, target, detail string, err error) {
	status := "ok"
	if err != nil {
		status = "error"
		if detail == "" {
			detail = err.Error()
		} else {
			detail = detail + ": " + err.Error()
		}
	}

	if dbErr := l.db.LogActivity(username, action, target, detail, status); dbErr != nil {
		l.log.Warn("could not write activity entry", "err", dbErr, "action", action)
	}
}

// System records an action performed by the console itself.
func (l *Logger) System(action, target, detail string, err error) {
	l.Record("system", action, target, detail, err)
}
