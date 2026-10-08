package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/instances"
)

// jobWorkerCount is set by main so the settings page can display it.
var jobWorkerCount = 2

// SetJobWorkerCount records the configured worker pool size for display.
func SetJobWorkerCount(n int) {
	if n > 0 {
		jobWorkerCount = n
	}
}

func (s *Server) handleDomainCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	username := usernameOf(userFrom(r))

	domain := strings.TrimSpace(r.FormValue("domain"))
	port := atoiDefault(r.FormValue("port"), 80)

	err := checkWriteAccess(r)
	if err == nil {
		err = s.Domains.Add(r.Context(), name, domain, port)
	}
	s.App.Activity.Record(username, "Add domain", name, domain, err)

	s.finish(w, r, s.InstanceURL(name)+"?tab=domains", err, "Added "+domain+" to "+name+".")
}

func (s *Server) handleInstanceDomainDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id := int64(atoiDefault(r.PathValue("id"), 0))
	username := usernameOf(userFrom(r))

	err := checkWriteAccess(r)
	if err == nil {
		err = s.Domains.Delete(r.Context(), id)
	}
	s.App.Activity.Record(username, "Remove domain", name, "", err)

	s.finish(w, r, s.InstanceURL(name)+"?tab=domains", err, "Domain removed.")
}

func (s *Server) handleDomainDelete(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	username := usernameOf(userFrom(r))

	// Remember the target so the audit entry is meaningful.
	label := ""
	if row, err := s.App.DB.GetDomain(id); err == nil {
		label = row.Domain
	}

	err := checkWriteAccess(r)
	if err == nil {
		err = s.Domains.Delete(r.Context(), id)
	}
	s.App.Activity.Record(username, "Remove domain", label, "", err)

	s.finish(w, r, "/domains", err, "Removed "+label+".")
}

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	list, err := s.Domains.List(r.Context())
	if err != nil {
		s.renderError(w, r, "Domains & DNS", "domains", err)
		return
	}

	data := domainsData{
		baseData:     s.newBase(w, r, "domains", "Domains & DNS"),
		Domains:      list,
		CaddyPreview: s.Domains.PreviewCaddyfile(r.Context()),
		CaddyEnabled: s.Domains.CaddyEnabled(),
		PublicIP:     strings.TrimSpace(s.App.DB.GetSetting("network.public_ip", "")),
	}
	s.render(w, r, "domains", http.StatusOK, data)
}

// handleActivity renders the audit log with simple pagination.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	const limit = 50

	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}

	total, err := s.App.DB.CountActivity()
	if err != nil {
		s.renderError(w, r, "Activity", "activity", err)
		return
	}

	items, err := s.App.DB.ListActivity(limit, (page-1)*limit)
	if err != nil {
		s.renderError(w, r, "Activity", "activity", err)
		return
	}

	jobs, err := s.App.DB.ListJobs(15)
	if err != nil {
		s.log.Warn("could not list jobs", "err", err)
	}

	pages := (total + limit - 1) / limit
	if pages < 1 {
		pages = 1
	}

	data := activityData{
		baseData: s.newBase(w, r, "activity", "Activity"),
		Activity: items,
		Total:    total,
		Page:     page,
		Pages:    pages,
		Limit:    limit,
		Jobs:     jobs,
	}
	s.render(w, r, "activity", http.StatusOK, data)
}

// handleActiveJobs renders just the job tracker for HTMX polling.
func (s *Server) handleActiveJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.App.DB.ListActiveJobs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.Renderer.RenderPartial(w, "dashboard", "job_list", jobsData{Jobs: jobs}); err != nil {
		s.log.Error("render jobs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// handleJobsStream pushes job tracker updates over Server-Sent Events.
//
// The job tracker historically polled GET /jobs/active every 2s (15s when
// idle), visible in DevTools as a constant request stream. SSE keeps one
// long-lived request open instead: the server re-renders the same job_list
// partial only when the active set actually changes and pushes it as a
// `jobs` event. The browser client in static/app.js swaps #job-tracker and
// disables the HTMX polling attributes once the stream is live.
func (s *Server) handleJobsStream(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()

	fmt.Fprintf(w, "retry: 3000\n\n")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	last := ""
	// Send the current state immediately so a fresh page does not wait.
	if html, fingerprint, err := s.renderJobTracker(); err == nil {
		last = fingerprint
		writeSSE(w, "jobs", html)
		flusher.Flush()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.streamCtx.Done():
			// Server shutdown: exit so http.Server.Shutdown does not
			// wait out its timeout on this long-lived stream.
			return
		case <-heartbeat.C:
			// Comment keeps proxies and the browser from timing out an
			// idle stream when no jobs are running.
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-ticker.C:
			html, fingerprint, err := s.renderJobTracker()
			if err != nil {
				continue
			}
			if fingerprint == last {
				continue
			}
			last = fingerprint
			writeSSE(w, "jobs", html)
			flusher.Flush()
		}
	}
}

// renderJobTracker renders the job_list partial and returns its HTML plus a
// fingerprint used to detect changes between polls.
func (s *Server) renderJobTracker() (string, string, error) {
	jobs, err := s.App.DB.ListActiveJobs()
	if err != nil {
		return "", "", err
	}

	var buf strings.Builder
	if err := s.Renderer.ExecutePartial(&buf, "dashboard", "job_list", jobsData{Jobs: jobs}); err != nil {
		return "", "", err
	}

	html := buf.String()

	var fp strings.Builder
	for _, j := range jobs {
		fmt.Fprintf(&fp, "%s|%s|%d|%s|%s;", j.ID, j.Status, j.Progress, j.Message, j.Error)
	}

	return html, fp.String(), nil
}

// writeSSE writes one named event. Multi-line HTML becomes multiple data:
// lines; a trailing blank line dispatches the event.
func writeSSE(w http.ResponseWriter, event, html string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(strings.ReplaceAll(html, "\r", ""), "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprintf(w, "\n")
}

// handleStorage shows storage pools with the instances they host.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	host, err := s.Monitoring.HostStats()
	if err != nil {
		s.renderError(w, r, "Storage", "storage", err)
		return
	}

	list, err := s.Instances.List(r.Context())
	if err != nil {
		s.renderError(w, r, "Storage", "storage", err)
		return
	}

	byPool := map[string][]instances.Instance{}
	for _, inst := range list {
		byPool[inst.StoragePool] = append(byPool[inst.StoragePool], inst)
	}

	view := &hostView{Host: host}
	for _, pool := range host.Pools {
		item := poolView{
			Name:      pool.Name,
			Driver:    pool.Driver,
			Used:      pool.Used,
			Total:     pool.Total,
			Instances: byPool[pool.Name],
		}
		if item.Total > item.Used {
			item.Free = item.Total - item.Used
		}
		view.Pools = append(view.Pools, item)
		delete(byPool, pool.Name)
	}

	data := storageData{
		baseData: s.newBase(w, r, "storage", "Storage"),
		Host:     view,
	}
	s.render(w, r, "storage", http.StatusOK, data)
}

// handleNetworking lists Incus networks plus each instance's address.
func (s *Server) handleNetworking(w http.ResponseWriter, r *http.Request) {
	networks := []networkInfo{}
	if raw, err := s.App.Incus.Networks(); err == nil {
		for _, n := range raw {
			address := n.Config["ipv4.address"]
			if address == "" {
				address = n.Config["ipv6.address"]
			}

			managed := "no"
			if n.Managed {
				managed = "yes"
			}

			networks = append(networks, networkInfo{
				Name:        n.Name,
				Type:        n.Type,
				Managed:     managed,
				Address:     address,
				UsedBy:      len(n.UsedBy),
				Description: n.Description,
			})
		}
	} else {
		s.log.Warn("could not list networks", "err", err)
	}

	list, err := s.Instances.List(r.Context())
	if err != nil {
		s.renderError(w, r, "Networking", "networking", err)
		return
	}

	data := networkingData{
		baseData:     s.newBase(w, r, "networking", "Networking"),
		Networks:     networks,
		Instances:    list,
		CaddyEnabled: s.Domains.CaddyEnabled(),
		CaddyPath:    s.App.Cfg.CaddyConfigPath,
		CaddyAdmin:   s.App.Cfg.CaddyAdminURL,
	}
	s.render(w, r, "networking", http.StatusOK, data)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	host, err := s.Monitoring.HostStats()
	if err != nil {
		s.log.Warn("could not read host stats", "err", err)
		host = &monitoringHostFallback
	}

	users, err := s.App.DB.ListUsers()
	if err != nil {
		s.renderError(w, r, "Settings", "settings", err)
		return
	}

	sessions, err := s.App.DB.CountSessions()
	if err != nil {
		s.log.Warn("could not count sessions", "err", err)
	}

	data := settingsData{
		baseData: s.newBase(w, r, "settings", "Settings"),
		Host:     host,
		Users:    users,

		S3Configured:      s.App.Cfg.S3Configured(),
		S3Bucket:          s.App.Cfg.S3Bucket,
		S3Endpoint:        s.App.Cfg.S3Endpoint,
		S3Region:          s.App.Cfg.S3Region,
		S3Prefix:          s.App.Cfg.S3Prefix,
		S3AccessKeyMasked: maskSecret(s.App.Cfg.S3AccessKey),

		CaddyEnabled:   s.Domains.CaddyEnabled(),
		CaddyPath:      s.App.Cfg.CaddyConfigPath,
		CaddyAdmin:     s.App.Cfg.CaddyAdminURL,
		CaddyReloadCmd: s.App.Cfg.CaddyReloadCmd,
		CaddyPreview:   s.Domains.PreviewCaddyfile(r.Context()),

		SessionCount: sessions,
		SessionTTL:   s.App.Cfg.SessionTTL,

		MetricsInterval:  s.App.Cfg.MetricsIntervalSeconds,
		MetricsRetention: s.App.Cfg.MetricsRetentionHours,
		JobWorkers:       jobWorkerCount,

		IncusProject: s.App.Cfg.IncusProject,
	}

	if list, err := s.Instances.List(r.Context()); err == nil {
		running := 0
		for _, i := range list {
			if i.Running {
				running++
			}
		}
		data.InstanceCount = len(list)
		data.RunningCount = running
	}

	// Two-factor state for the signed-in user. A pending setup secret is
	// shown once, right after generation.
	if me := userFrom(r); me != nil {
		if fresh, err := s.App.DB.GetUser(me.ID); err == nil {
			data.TOTPEnabled = fresh.TOTPEnabled
			if r.URL.Query().Get("totp") == "setup" && !fresh.TOTPEnabled && fresh.TOTPSecret != "" {
				data.TOTPSetupSecret = fresh.TOTPSecret
				data.TOTPSetupURL = auth.ProvisioningURL(fresh.Username, fresh.TOTPSecret)
				data.TOTPSetupQR = totpQRCode(data.TOTPSetupURL)
			}
		}
	}

	s.render(w, r, "settings", http.StatusOK, data)
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")

	if err := checkAdminAccess(r); err != nil {
		s.App.Activity.Record(actor.Username, "Create user", username, role, err)
		s.finish(w, r, "/settings", err, "")
		return
	}

	err := s.createUser(username, password, role)
	s.App.Activity.Record(actor.Username, "Create user", username, role, err)

	if isHTMX(r) {
		s.serveUsersFragments(w, r, err, "User "+username+" created.")
		return
	}
	s.finish(w, r, "/settings", err, "User "+username+" created.")
}

// serveUsersFragments renders the HTMX response for user create/delete: the
// fresh users list plus out-of-band swaps for the flash and the user count.
func (s *Server) serveUsersFragments(w http.ResponseWriter, r *http.Request, actionErr error, notice string) {
	data := usersFragmentData{CSRF: csrfFrom(r), User: userFrom(r)}
	if actionErr != nil {
		data.Error = friendlyError(actionErr)
	} else {
		data.Notice = notice
	}

	users, err := s.App.DB.ListUsers()
	if err != nil {
		data.Error = friendlyError(err)
	} else {
		data.Users = users
	}

	if err := s.Renderer.RenderPartial(w, "settings", "users_response", data); err != nil {
		s.log.Error("render users list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// handleUserDelete removes a console user. Only admins may delete users, and
// safety rails protect the actor's own account and the last admin account.
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.PathValue("id"), 0))
	target, err := s.App.DB.GetUser(id)
	var notice string
	if err == nil {
		if aerr := checkAdminAccess(r); aerr != nil {
			err = aerr
		} else if target.ID == actor.ID {
			err = errors.New("you cannot delete your own account")
		} else if target.Role == auth.RoleAdmin && s.adminCountExcept(target.ID) == 0 {
			err = errors.New("cannot delete the last admin account")
		} else {
			// Sessions cascade on delete, but clear them explicitly in case
			// foreign keys are ever disabled.
			_ = s.App.DB.DeleteUserSessions(target.ID)
			if derr := s.App.DB.DeleteUser(target.ID); derr != nil {
				err = derr
			} else {
				notice = "User " + target.Username + " deleted."
			}
		}
	}
	label := ""
	if target != nil {
		label = target.Username
	}
	s.App.Activity.Record(actor.Username, "Delete user", label, "", err)

	if isHTMX(r) {
		s.serveUsersFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, notice)
}

// adminCountExcept counts admin accounts other than the given id.
func (s *Server) adminCountExcept(exceptID int64) int {
	users, err := s.App.DB.ListUsers()
	if err != nil {
		return 0
	}
	count := 0
	for _, u := range users {
		if u.Role == auth.RoleAdmin && u.ID != exceptID {
			count++
		}
	}
	return count
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	current := r.FormValue("current_password")
	next := r.FormValue("new_password")

	err := s.App.Auth.ChangePassword(user.ID, current, next)
	s.App.Activity.Record(user.Username, "Change password", user.Username, "", err)

	if err != nil {
		if isHTMX(r) {
			s.serveSettingsFlash(w, err)
			return
		}
		s.fail(w, r, "/settings", err)
		return
	}

	// Changing the password invalidates every session, including this one,
	// so the browser must sign in again.
	s.clearCookie(w, sessionCookie)
	s.setFlash(w, "ok", "Password changed. Please sign in again.")
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// serveSettingsFlash renders a flash-only out-of-band swap for settings
// forms that have no other fragment to refresh.
func (s *Server) serveSettingsFlash(w http.ResponseWriter, actionErr error) {
	data := usersFragmentData{CSRF: "", Error: friendlyError(actionErr)}
	// Users and User stay empty: only the flash part of the response is used.
	if err := s.Renderer.RenderPartial(w, "settings", "settings_flash_oob", data); err != nil {
		s.log.Error("render settings flash", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// handleTOTPSetup generates a fresh secret and shows it once for the user
// to enter into their authenticator app. Nothing is enforced yet.
func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	secret, _, err := auth.GenerateTOTPSecret(user.Username)
	if err != nil {
		if isHTMX(r) {
			s.serveSettingsFlash(w, err)
			return
		}
		s.fail(w, r, "/settings", err)
		return
	}
	if err := s.App.DB.SetTOTPSecret(user.ID, secret); err != nil {
		if isHTMX(r) {
			s.serveSettingsFlash(w, err)
			return
		}
		s.fail(w, r, "/settings", err)
		return
	}

	s.App.Activity.Record(user.Username, "Start two-factor setup", user.Username, "", nil)
	if isHTMX(r) {
		s.serveTOTPFragments(w, r, nil, "")
		return
	}
	http.Redirect(w, r, "/settings?totp=setup", http.StatusSeeOther)
}

// handleTOTPEnable confirms a code from the authenticator app and turns
// enforcement on.
func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	fresh, err := s.App.DB.GetUser(user.ID)
	if err != nil {
		if isHTMX(r) {
			s.serveSettingsFlash(w, err)
			return
		}
		s.fail(w, r, "/settings", err)
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	if !auth.VerifyTOTPCode(fresh.TOTPSecret, code) {
		badCode := errors.New("incorrect code — try the current code from your app")
		s.App.Activity.Record(user.Username, "Confirm two-factor", user.Username, "", errors.New("invalid code"))
		if isHTMX(r) {
			// Keep showing the pending secret so the user can retry the code.
			s.serveTOTPFragments(w, r, badCode, "")
			return
		}
		s.fail(w, r, "/settings?totp=setup", badCode)
		return
	}

	if err := s.App.DB.SetTOTPEnabled(user.ID, true); err != nil {
		if isHTMX(r) {
			s.serveTOTPFragments(w, r, err, "")
			return
		}
		s.fail(w, r, "/settings", err)
		return
	}

	s.App.Activity.Record(user.Username, "Enable two-factor", user.Username, "", nil)
	if isHTMX(r) {
		s.serveTOTPFragments(w, r, nil, "Two-factor authentication is on.")
		return
	}
	s.succeed(w, r, "/settings", "Two-factor authentication is on.")
}

// handleTOTPDisable turns enforcement off after confirming the password.
// The secret is cleared entirely.
func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	fresh, err := s.App.DB.GetUser(user.ID)
	if err != nil {
		if isHTMX(r) {
			s.serveSettingsFlash(w, err)
			return
		}
		s.fail(w, r, "/settings", err)
		return
	}
	if !auth.VerifyPassword(fresh.PasswordHash, r.FormValue("current_password")) {
		badPassword := errors.New("incorrect password")
		s.App.Activity.Record(user.Username, "Disable two-factor", user.Username, "", auth.ErrInvalidCredentials)
		if isHTMX(r) {
			s.serveTOTPFragments(w, r, badPassword, "")
			return
		}
		s.fail(w, r, "/settings", badPassword)
		return
	}

	if err := s.App.DB.ClearTOTP(user.ID); err != nil {
		if isHTMX(r) {
			s.serveTOTPFragments(w, r, err, "")
			return
		}
		s.fail(w, r, "/settings", err)
		return
	}

	s.App.Activity.Record(user.Username, "Disable two-factor", user.Username, "", nil)
	if isHTMX(r) {
		s.serveTOTPFragments(w, r, nil, "Two-factor authentication is off.")
		return
	}
	s.succeed(w, r, "/settings", "Two-factor authentication is off.")
}

// serveTOTPFragments renders the HTMX response for two-factor operations:
// the fresh TOTP card plus an out-of-band flash. A pending (unconfirmed)
// secret is shown again so a mistyped code can simply be retried.
func (s *Server) serveTOTPFragments(w http.ResponseWriter, r *http.Request, actionErr error, notice string) {
	user := userFrom(r)
	data := totpFragmentData{CSRF: csrfFrom(r)}
	if actionErr != nil {
		data.Error = friendlyError(actionErr)
	} else {
		data.Notice = notice
	}

	if user != nil {
		if fresh, err := s.App.DB.GetUser(user.ID); err == nil {
			data.TOTPEnabled = fresh.TOTPEnabled
			if !fresh.TOTPEnabled && fresh.TOTPSecret != "" {
				data.TOTPSetupSecret = fresh.TOTPSecret
				data.TOTPSetupURL = auth.ProvisioningURL(fresh.Username, fresh.TOTPSecret)
				data.TOTPSetupQR = totpQRCode(data.TOTPSetupURL)
			}
		}
	}

	if err := s.Renderer.RenderPartial(w, "settings", "totp_response", data); err != nil {
		s.log.Error("render totp card", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// totpQRCode renders the setup URL as a scannable PNG data URI. An empty
// string on failure leaves manual entry, which always works.
func totpQRCode(otpURL string) string {
	if otpURL == "" {
		return ""
	}
	png, err := qrcode.Encode(otpURL, qrcode.Medium, 256)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// maskSecret hides all but the first and last characters of a credential.
func maskSecret(value string) string {
	if value == "" {
		return "—"
	}
	if len(value) <= 6 {
		return strings.Repeat("\u2022", len(value))
	}
	return value[:3] + strings.Repeat("\u2022", 6) + value[len(value)-2:]
}

// createUser validates and stores a new console user.
func (s *Server) createUser(username, password, role string) error {
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 32 {
		return errors.New("usernames must be between 3 and 32 characters")
	}
	for _, r := range username {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !valid {
			return errors.New("usernames may only contain letters, digits, dots, underscores and hyphens")
		}
	}

	if len(password) < 8 {
		return errors.New("passwords must be at least 8 characters")
	}

	switch role {
	case auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer:
	default:
		return errors.New("unknown role")
	}

	if _, err := s.App.DB.GetUserByUsername(username); err == nil {
		return errors.New("that username already exists")
	} else if !errors.Is(err, database.ErrNotFound) {
		return err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	_, err = s.App.DB.CreateUser(username, hash, role)
	return err
}
