package web

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/backups"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/domains"
	"github.com/peaceful/cloud-console/internal/instances"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/monitoring"
	"github.com/peaceful/cloud-console/internal/snapshots"
)

const (
	sessionCookie = "console_session"
	csrfCookie    = "console_csrf"
	flashCookie   = "console_flash"
	sessionTTL    = 14 * 24 * time.Hour
)

// Deps is everything the HTTP layer needs, wired once in main.
type Deps struct {
	App        *core.App
	Renderer   *Renderer
	Instances  *instances.Service
	Domains    *domains.Service
	Snapshots  *snapshots.Service
	Backups    *backups.Service
	Monitoring *monitoring.Service

	Version string
	Dev     bool
}

// Server is the HTTP server.
type Server struct {
	Deps
	log *slog.Logger
	mux *http.ServeMux

	// streamCtx is cancelled on shutdown so long-lived SSE streams exit
	// instead of holding http.Server.Shutdown until its timeout expires.
	streamCtx   context.Context
	stopStreams context.CancelFunc
}

// New builds the server and registers every route.
func New(deps Deps) *Server {
	s := &Server{Deps: deps, log: deps.App.Log, mux: http.NewServeMux()}
	s.streamCtx, s.stopStreams = context.WithCancel(context.Background())
	s.routes()
	return s
}

// CloseEventStreams unblocks every open SSE stream. Call it before
// http.Server.Shutdown so Ctrl+C shuts down promptly instead of waiting out
// the shutdown timeout with a fatal "context deadline exceeded".
func (s *Server) CloseEventStreams() {
	s.stopStreams()
}

// Handler returns the middleware-wrapped handler.
func (s *Server) Handler(static http.Handler) http.Handler {
	mux := http.NewServeMux()

	// Static assets are served without authentication.
	mux.Handle("GET /static/", http.StripPrefix("/static/", static))
	mux.Handle("GET /healthz", http.HandlerFunc(s.health))

	// Everything else goes through the full middleware chain.
	mux.Handle("/", s.mux)

	return s.withLogging(s.withCSRF(s.withAuth(mux)))
}

func (s *Server) routes() {
	m := s.mux

	m.HandleFunc("GET /{$}", s.handleDashboard)

	m.HandleFunc("GET /login", s.handleLoginPage)
	m.HandleFunc("POST /login", s.handleLoginSubmit)
	m.HandleFunc("GET /login/totp", s.handleTOTPPage)
	m.HandleFunc("POST /login/totp", s.handleLoginTOTPSubmit)
	m.HandleFunc("POST /logout", s.handleLogout)

	m.HandleFunc("GET /instances", s.handleInstances)
	m.HandleFunc("GET /create", s.handleCreatePage)
	m.HandleFunc("GET /create/blueprints", s.handleBlueprints)
	m.HandleFunc("POST /instances", s.handleCreateSubmit)

	m.HandleFunc("GET /instances/{name}", s.handleInstance)
	m.HandleFunc("GET /instances/{name}/tab/{tab}", s.handleInstanceTab)
	m.HandleFunc("GET /instances/{name}/header", s.handleInstanceHeader)

	m.HandleFunc("POST /instances/{name}/state", s.handleInstanceState)
	m.HandleFunc("POST /instances/{name}/delete", s.handleInstanceDelete)
	m.HandleFunc("POST /instances/{name}/rename", s.handleInstanceRename)
	m.HandleFunc("POST /instances/{name}/rebuild", s.handleInstanceRebuild)
	m.HandleFunc("POST /instances/{name}/limits", s.handleInstanceLimits)
	m.HandleFunc("POST /instances/{name}/notes", s.handleInstanceNotes)

	m.HandleFunc("POST /instances/{name}/snapshots", s.handleSnapshotCreate)
	m.HandleFunc("POST /instances/{name}/snapshots/{snapshot}/delete", s.handleSnapshotDelete)
	m.HandleFunc("POST /instances/{name}/snapshots/{snapshot}/restore", s.handleSnapshotRestore)

	m.HandleFunc("POST /instances/{name}/backups", s.handleBackupCreate)
	m.HandleFunc("POST /instances/{name}/backups/{backup}/delete", s.handleBackupDelete)
	m.HandleFunc("POST /instances/{name}/backups/{backup}/restore", s.handleBackupRestore)
	m.HandleFunc("POST /instances/{name}/backups/{backup}/export", s.handleBackupExport)

	m.HandleFunc("POST /instances/{name}/domains", s.handleDomainCreate)
	m.HandleFunc("POST /instances/{name}/domains/{id}/delete", s.handleInstanceDomainDelete)
	m.HandleFunc("POST /domains/{id}/delete", s.handleDomainDelete)

	m.HandleFunc("GET /snapshots", s.handleSnapshots)
	m.HandleFunc("GET /backups", s.handleBackups)
	m.HandleFunc("GET /domains", s.handleDomains)
	m.HandleFunc("GET /storage", s.handleStorage)
	m.HandleFunc("GET /networking", s.handleNetworking)
	m.HandleFunc("GET /activity", s.handleActivity)
	m.HandleFunc("GET /settings", s.handleSettings)

	m.HandleFunc("POST /settings/users", s.handleUserCreate)
	m.HandleFunc("POST /settings/users/{id}/delete", s.handleUserDelete)
	m.HandleFunc("POST /settings/password", s.handlePasswordChange)
	m.HandleFunc("POST /settings/totp/setup", s.handleTOTPSetup)
	m.HandleFunc("POST /settings/totp/enable", s.handleTOTPEnable)
	m.HandleFunc("POST /settings/totp/disable", s.handleTOTPDisable)

	m.HandleFunc("GET /jobs/active", s.handleActiveJobs)
	m.HandleFunc("GET /jobs/stream", s.handleJobsStream)
}

// --- middleware -----------------------------------------------------------

// withLogging records every request, skipping noisy static asset hits.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}

		if !strings.HasPrefix(r.URL.Path, "/static/") {
			s.log.Log(r.Context(), level, "http request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration", time.Since(start).Round(time.Millisecond).String())
		}
	})
}

// withAuth resolves the session and rejects unauthenticated requests.
func (s *Server) withAuth(next http.Handler) http.Handler {
	public := map[string]bool{
		"/login":      true,
		"/login/totp": true,
		"/healthz":    true,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err == nil && cookie.Value != "" {
			if user, err := s.App.Auth.Authenticate(cookie.Value); err == nil {
				ctx := context.WithValue(r.Context(), userKey{}, user)
				ctx = context.WithValue(ctx, tokenKey{}, cookie.Value)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			s.clearCookie(w, sessionCookie)
		}

		if public[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		// Static assets must load on the login page itself, before any
		// session exists. (Previously only exact paths were public, so
		// /static/* redirected to /login and the sign-in page rendered
		// unstyled.)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		// HTMX requests get a redirect instruction instead of a full page.
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// EventSource (SSE) cannot follow a login redirect usefully; tell
		// the client to stop retrying so the HTMX polling fallback takes over.
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		target := "/login"
		if r.URL.Path != "/" {
			target += "?next=" + url.QueryEscape(r.URL.RequestURI())
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	})
}

// withCSRF issues and verifies the double-submit CSRF token.
//
// The token lives in an HttpOnly cookie and is echoed into every form, so a
// cross-site page cannot forge a valid submission.
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
			token = c.Value
		}

		if token == "" {
			generated, err := auth.RandomPassword(40)
			if err != nil {
				s.log.Error("could not generate csrf token", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			token = generated
			http.SetCookie(w, &http.Cookie{
				Name:     csrfCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Secure:   s.App.Cfg.SecureCookies,
				MaxAge:   int((30 * 24 * time.Hour).Seconds()),
			})
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			supplied := r.FormValue("csrf_token")
			if supplied == "" {
				supplied = r.Header.Get("X-CSRF-Token")
			}
			if subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
				s.log.Warn("csrf validation failed", "path", r.URL.Path, "method", r.Method)
				http.Error(w, "invalid or expired form token — reload the page and try again", http.StatusForbidden)
				return
			}
		}

		ctx := context.WithValue(r.Context(), csrfKey{}, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards SSE flushes. Without it, withLogging's wrapper hides the
// underlying Flusher and GET /jobs/stream fails with 500.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// --- context keys ---------------------------------------------------------

type userKey struct{}
type tokenKey struct{}
type csrfKey struct{}

func userFrom(r *http.Request) *models.User {
	user, _ := r.Context().Value(userKey{}).(*models.User)
	return user
}

func tokenFrom(r *http.Request) string {
	token, _ := r.Context().Value(tokenKey{}).(string)
	return token
}

func csrfFrom(r *http.Request) string {
	token, _ := r.Context().Value(csrfKey{}).(string)
	return token
}

// --- flash messages -------------------------------------------------------

// setFlash stores a one-shot message in a cookie for the next page load.
func (s *Server) setFlash(w http.ResponseWriter, kind, message string) {
	if message == "" {
		return
	}
	value := kind + "|" + base64.RawURLEncoding.EncodeToString([]byte(message))
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.App.Cfg.SecureCookies,
		MaxAge:   60,
	})
}

// takeFlash reads and clears the flash cookie.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) (kind, message string) {
	cookie, err := r.Cookie(flashCookie)
	if err != nil || cookie.Value == "" {
		return "", ""
	}

	s.clearCookie(w, flashCookie)

	parts := strings.SplitN(cookie.Value, "|", 2)
	if len(parts) != 2 {
		return "", ""
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	return parts[0], string(raw)
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.App.Cfg.SecureCookies,
		MaxAge:   -1,
	})
}

// --- helpers --------------------------------------------------------------

// newBase seeds the fields shared by every layout-rendered page.
func (s *Server) newBase(w http.ResponseWriter, r *http.Request, nav, title string) baseData {
	kind, message := s.takeFlash(w, r)

	base := baseData{
		Title:       title,
		Nav:         nav,
		CSRF:        csrfFrom(r),
		User:        userFrom(r),
		Version:     s.Version,
		IncusSocket: s.App.Incus.Socket(),
		Query:       r.URL.Query().Get("q"),
	}

	switch kind {
	case "ok":
		base.Notice = message
	case "err":
		base.Error = message
	case "warn":
		base.Error = message
	}

	if count, err := s.App.DB.CountInstanceMeta(); err == nil {
		base.InstanceCount = count
	}

	return base
}

// withJobs attaches the active jobs so the tracker can render inline.
func (s *Server) withJobs(base baseData) baseData {
	if jobs, err := s.App.DB.ListActiveJobs(); err == nil {
		base.Jobs = jobs
	}
	return base
}

// redirectBack sends the browser back to the page that submitted a form.
func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, fallback string) {
	target := fallback

	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && (u.Host == "" || u.Host == r.Host) {
			target = u.Path
			if u.RawQuery != "" {
				target += "?" + u.RawQuery
			}
		}
	}

	http.Redirect(w, r, target, http.StatusSeeOther)
}

// fail reports an error on a form submission: flash + redirect back.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, fallback string, err error) {
	s.log.Warn("request failed", "path", r.URL.Path, "err", err)
	s.setFlash(w, "err", friendlyError(err))
	s.redirectBack(w, r, fallback)
}

// errReadOnly is reported when a viewer role attempts a mutation.
var errReadOnly = errors.New("your account has read-only access — ask an admin to make this change")

// errAdminOnly is reported when a non-admin attempts user management.
var errAdminOnly = errors.New("admin access is required for this action")

// checkWriteAccess blocks read-only (viewer) roles from mutating anything.
// Every infrastructure-changing handler must call it before acting.
func checkWriteAccess(r *http.Request) error {
	if user := userFrom(r); user == nil || !auth.CanWrite(user.Role) {
		return errReadOnly
	}
	return nil
}

// checkAdminAccess blocks non-admin roles from managing console users.
func checkAdminAccess(r *http.Request) error {
	if user := userFrom(r); user == nil || !auth.CanAdmin(user.Role) {
		return errAdminOnly
	}
	return nil
}

// succeed reports a successful mutation and redirects back.
func (s *Server) succeed(w http.ResponseWriter, r *http.Request, fallback, message string) {
	s.setFlash(w, "ok", message)
	s.redirectBack(w, r, fallback)
}

// friendlyError turns low-level errors into short messages for the UI.
func friendlyError(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()
	switch {
	case errors.Is(err, instances.ErrNotFound), errors.Is(err, backups.ErrNotFound),
		errors.Is(err, snapshots.ErrNotFound), errors.Is(err, domains.ErrNotFound):
		return "That object no longer exists."
	case errors.Is(err, instances.ErrInvalidName):
		return "Instance names must be lowercase letters, digits and hyphens."
	case errors.Is(err, instances.ErrNameInUse):
		return "That name is already taken."
	case errors.Is(err, instances.ErrInvalidSpec):
		return msg
	default:
		return msg
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if _, err := s.App.Incus.Server(); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "incus unreachable: %v\n", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}
