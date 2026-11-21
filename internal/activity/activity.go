// Package activity writes the console's audit log.
//
// Every mutating action a user takes is recorded so the Instances → History tab
// and the global Activity page can show what happened, who did it and whether it
// worked. Each entry is filed under the tenant it concerns: the owner of the
// target instance, or the acting user when the action is not about an instance.
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

	if dbErr := l.db.LogActivity(l.ownerFor(username, target), username, action, target, detail, status); dbErr != nil {
		l.log.Warn("could not write activity entry", "err", dbErr, "action", action)
	}
}

// System records an action performed by the console itself.
func (l *Logger) System(action, target, detail string, err error) {
	l.Record("system", action, target, detail, err)
}

// ownerFor resolves the tenant an entry belongs to. An action on an instance
// belongs to that instance's owner even when an administrator performed it, so
// the tenant's history still shows what happened. Everything else belongs to
// the acting user; unknown actors (the console itself) stay admin-only.
func (l *Logger) ownerFor(username, target string) int64 {
	if target != "" {
		if meta, err := l.db.GetInstanceMeta(target); err == nil {
			return meta.OwnerID
		}
	}
	if username == "" || username == "system" {
		return 0
	}
	if user, err := l.db.GetUserByUsername(username); err == nil {
		return user.ID
	}
	return 0
}
