// Command cloud-console — the Peaceful Cloud Console — is a small,
// Lightsail-style web UI for managing Incus instances on a single physical
// server.
//
// It ships as one binary: templates, CSS and JavaScript are embedded, SQLite
// stores console metadata, and Incus is reached over its local Unix socket.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/peaceful/cloud-console/internal/activity"
	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/backups"
	"github.com/peaceful/cloud-console/internal/caddy"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/domains"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/instances"
	"github.com/peaceful/cloud-console/internal/jobs"
	"github.com/peaceful/cloud-console/internal/monitoring"
	"github.com/peaceful/cloud-console/internal/snapshots"
	"github.com/peaceful/cloud-console/internal/web"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

//go:embed templates static
var assets embed.FS

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr     = flag.String("addr", "", "HTTP listen address (overrides CONSOLE_ADDR)")
		socket   = flag.String("socket", "", "path to the Incus Unix socket (overrides INCUS_SOCKET)")
		workers  = flag.Int("workers", 3, "number of background job workers")
		dev      = flag.Bool("dev", false, "load templates and static files from disk")
		logLevel = flag.String("log-level", "info", "log level: debug, info, warn, error")
		showVer  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("cloud-console", version)
		return nil
	}

	log := newLogger(*logLevel)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.EnvFile != "" {
		log.Info("loaded environment file", "path", cfg.EnvFile)
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	if *socket != "" {
		cfg.IncusSocket = *socket
	}
	if envDev() {
		*dev = true
	}
	if *dev {
		log.Info("development mode: templates and static files are read from disk")
	}

	// --- data layer -------------------------------------------------------

	db, err := database.Open(cfg.DBPath, log)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	// --- incus ------------------------------------------------------------

	log.Info("connecting to incus", "socket", cfg.IncusSocket)
	incusClient, err := incus.Connect(cfg.IncusSocket, cfg.IncusProject, log)
	if err != nil {
		return fmt.Errorf("%w\n\n"+
			"Is incusd running and is this user allowed to read %s?\n"+
			"Set INCUS_SOCKET to point somewhere else.", err, cfg.IncusSocket)
	}

	if server, err := incusClient.Server(); err == nil && server != nil {
		log.Info("connected to incus",
			"server", server.Environment.ServerName,
			"version", server.Environment.ServerVersion,
			"driver", server.Environment.Driver)
	}

	// --- services ---------------------------------------------------------

	activityLog := activity.New(db, log)
	authService := auth.New(db, cfg, log)

	if password, err := authService.EnsureAdmin(); err != nil {
		return fmt.Errorf("create bootstrap administrator: %w", err)
	} else if password != "" {
		log.Warn("created the initial administrator account",
			"username", cfg.AdminUser, "password", password,
			"note", "store this now and change it after signing in")
	}

	caddyManager := caddy.New(cfg, log)
	if !caddyManager.Enabled() {
		log.Warn("caddy management is disabled: set CADDY_CONFIG_PATH or CADDY_ADMIN_URL to route domains automatically")
	}

	jobManager := jobs.New(db, log, *workers)

	app := &core.App{
		Cfg:      cfg,
		DB:       db,
		Incus:    incusClient,
		Caddy:    caddyManager,
		Jobs:     jobManager,
		Auth:     authService,
		Activity: activityLog,
		Log:      log,
	}

	instancesService := instances.New(app)
	domainsService := domains.New(app)
	snapshotsService := snapshots.New(app)
	backupsService := backups.New(app)
	monitoringService := monitoring.New(app)

	// Caddy is re-rendered whenever instances or domains change.
	instancesService.SetNotifier(domainsService)
	backupsService.SetNotifier(domainsService)

	instancesService.RegisterJobHandlers()
	backupsService.RegisterJobHandlers()

	// --- http -------------------------------------------------------------

	rendererFS, staticHandler, err := assetSources(assets, *dev)
	if err != nil {
		return err
	}

	renderer, err := web.NewRenderer(rendererFS, *dev, log)
	if err != nil {
		return fmt.Errorf("load templates: %w", err)
	}

	web.SetJobWorkerCount(*workers)

	server := web.New(web.Deps{
		App:        app,
		Renderer:   renderer,
		Instances:  instancesService,
		Domains:    domainsService,
		Snapshots:  snapshotsService,
		Backups:    backupsService,
		Monitoring: monitoringService,
		Version:    version,
		Dev:        *dev,
	})

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Handler(staticHandler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// --- background work --------------------------------------------------

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := jobManager.Start(ctx); err != nil {
		return fmt.Errorf("start job workers: %w", err)
	}
	defer jobManager.Stop()

	go monitoringService.Run(ctx)
	go housekeeping(ctx, db, log)
	go reconcile(ctx, domainsService, log)

	// Serve the initial Caddyfile so a fresh install is immediately consistent.
	if err := domainsService.Sync(ctx); err != nil {
		log.Warn("could not apply the initial caddy configuration", "err", err)
	}

	// --- serve ------------------------------------------------------------

	serveErr := make(chan error, 1)
	go func() {
		log.Info("peaceful cloud console listening", "addr", cfg.Addr, "url", cfg.BaseURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return httpServer.Shutdown(shutdownCtx)
}

// assetSources returns the template file system and the static file handler.
//
// In development the files are read from disk so edits take effect immediately;
// in production they come from the embedded file system.
func assetSources(embedded embed.FS, dev bool) (fs.FS, http.Handler, error) {
	if dev {
		return os.DirFS("."), http.FileServer(http.Dir("static")), nil
	}

	// Strip the cached-control headers; the console is a single-user tool.
	staticFS, err := fs.Sub(embedded, "static")
	if err != nil {
		return nil, nil, fmt.Errorf("locate embedded static files: %w", err)
	}

	handler := http.FileServer(http.FS(staticFS))
	return embedded, handler, nil
}

// housekeeping prunes expired sessions and old job rows.
func housekeeping(ctx context.Context, db *database.DB, log *slog.Logger) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := db.PurgeExpiredSessions(); err != nil {
				log.Warn("could not purge sessions", "err", err)
			} else if n > 0 {
				log.Debug("purged expired sessions", "count", n)
			}

			if err := db.PurgeJobs(500); err != nil {
				log.Warn("could not purge jobs", "err", err)
			}
		}
	}
}

// reconcile periodically re-applies domain routing so an instance that was
// offline when a domain was added picks it up once it comes back.
func reconcile(ctx context.Context, domainsService *domains.Service, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := domainsService.Sync(ctx); err != nil {
				log.Warn("periodic caddy sync failed", "err", err)
			}
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler)
}

func envDev() bool {
	v := strings.TrimSpace(os.Getenv("CONSOLE_DEV"))
	return v == "1" || strings.EqualFold(v, "true")
}
