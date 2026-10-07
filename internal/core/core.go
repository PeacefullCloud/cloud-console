// Package core holds the shared dependencies every service needs.
//
// Passing a single App value keeps the service packages free of import cycles
// and keeps main.go as the only place where wiring happens.
package core

import (
	"log/slog"

	"github.com/peaceful/cloud-console/internal/activity"
	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/caddy"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/jobs"
)

// App bundles the console's collaborators.
type App struct {
	Cfg      *config.Config
	DB       *database.DB
	Incus    *incus.Client
	Caddy    *caddy.Manager
	Jobs     *jobs.Manager
	Auth     *auth.Service
	Activity *activity.Logger
	Log      *slog.Logger
}
