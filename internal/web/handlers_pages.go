package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"io"
	"net/http"
	"sync"
	"time"
	"strconv"
	"strings"

	"github.com/skip2/go-qrcode"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/domains"
	"github.com/peaceful/cloud-console/internal/instances"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/sso"
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
		ctx, cancel := operationContext(r)
		defer cancel()
		err = s.Domains.Add(ctx, name, domain, port)
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
		// The {name} in the URL must be the domain's actual instance:
		// otherwise somebody with access to one instance could remove a
		// domain belonging to another by mixing the two path values.
		dom, derr := s.App.DB.GetDomain(id)
		if derr != nil || dom.InstanceName != name {
			err = domains.ErrNotFound
		}
	}
	if err == nil {
		ctx, cancel := operationContext(r)
		defer cancel()
		err = s.Domains.Delete(ctx, id)
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
		ctx, cancel := operationContext(r)
		defer cancel()
		err = s.Domains.Delete(ctx, id)
	}
	s.App.Activity.Record(username, "Remove domain", label, "", err)

	s.finish(w, r, "/domains", err, "Removed "+label+".")
}

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	list, err := s.Domains.List(r.Context(), scopeFrom(r))
	if err != nil {
		s.renderError(w, r, "Domains & DNS", "domains", err)
		return
	}

	data := domainsData{
		baseData:     s.newBase(w, r, "domains", "Domains & DNS"),
		Domains:      list,
		CaddyEnabled: s.Domains.CaddyEnabled(),
		PublicIP:     publicIPFor(s.App, r),
	}
	// The generated Caddyfile covers every route on the host.
	if checkAdminAccess(r) == nil {
		data.CaddyPreview = s.Domains.PreviewCaddyfile(r.Context())
	}
	s.render(w, r, "domains", http.StatusOK, data)
}

// handleTLSAsk is Caddy's on-demand TLS gate. Caddy calls it before issuing a
// certificate for a hostname; we answer 200 only for domains that are actually
// configured, so an attacker cannot burn Let's Encrypt rate limits with
// arbitrary Host headers.
func (s *Server) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	if domain == "" {
		http.Error(w, "missing domain", http.StatusBadRequest)
		return
	}
	if _, err := s.App.DB.GetDomainByName(domain); err != nil {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// publicIPFor resolves the shared public IP to show for DNS setup: the database
// setting, then CONSOLE_PUBLIC_IP, then the first non-private IPv4 on a host
// interface, and finally the host the browser used to reach the console.
func publicIPFor(app *core.App, r *http.Request) string {
	if v := strings.TrimSpace(app.DB.GetSetting("network.public_ip", "")); v != "" {
		return v
	}
	if v := strings.TrimSpace(app.Cfg.PublicIP); v != "" {
		return v
	}
	if v := detectPublicIP(); v != "" {
		return v
	}
	host := strings.TrimSpace(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

var (
	detectedPublicIP     string
	detectedPublicIPOnce sync.Once
)


// isPublicIPv4 reports whether ip is a globally routable IPv4 address, excluding
// loopback, private, link-local and carrier-grade NAT ranges.
func isPublicIPv4(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	// Carrier-grade NAT (RFC 6598) is not covered by IsPrivate in all
	// versions, but is never a public address.
	if _, cg, err := net.ParseCIDR("100.64.0.0/10"); err == nil && cg.Contains(ip) {
		return false
	}
	return true
}

// detectPublicIP returns a usable public IP: first a non-private IPv4 on a
// host interface, otherwise the address Cloudflare sees for this host (so
// NAT-mapped static IPs are picked up automatically).
func detectPublicIP() string {
	detectedPublicIPOnce.Do(func() {
		// 1. Local interfaces.
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, iface := range ifaces {
				addrs, err := iface.Addrs()
				if err != nil {
					continue
				}
				for _, a := range addrs {
					var ip net.IP
					switch v := a.(type) {
					case *net.IPNet:
						ip = v.IP
					case *net.IPAddr:
						ip = v.IP
					}
					if ip == nil || ip.IsLoopback() {
						continue
					}
					if ip4 := ip.To4(); ip4 != nil && isPublicIPv4(ip4) {
						detectedPublicIP = ip4.String()
						return
					}
				}
			}
		}

		// 2. Ask Cloudflare what public address it sees.
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("https://cloudflare.com/cdn-cgi/trace")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "ip=") {
				detectedPublicIP = strings.TrimSpace(strings.TrimPrefix(line, "ip="))
				return
			}
		}
	})
	return detectedPublicIP
}

// handleActivity renders the audit log with simple pagination.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	const limit = 50
	scope := scopeFrom(r)

	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}

	total, err := s.App.DB.CountActivityScoped(scope)
	if err != nil {
		s.renderError(w, r, "Activity", "activity", err)
		return
	}

	items, err := s.App.DB.ListActivityScoped(scope, limit, (page-1)*limit)
	if err != nil {
		s.renderError(w, r, "Activity", "activity", err)
		return
	}

	jobs, err := s.App.DB.ListJobsScoped(scope, 15)
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
	jobs, err := s.App.DB.ListActiveJobsScoped(scopeFrom(r))
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

	// http.Server.WriteTimeout would otherwise cut the stream a couple of
	// minutes in, and every browser tab would reconnect forever.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		s.log.Warn("could not lift the write deadline for the job stream", "err", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()

	fmt.Fprintf(w, "retry: 3000\n\n")
	flusher.Flush()

	frames, unsubscribe := s.jobFeed(scopeFrom(r)).subscribe(s.streamCtx)
	defer unsubscribe()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

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
		case f := <-frames:
			writeSSE(w, "jobs", f.HTML)
			flusher.Flush()
		}
	}
}

// jobFeed returns the one renderer behind every open job stream for a scope.
// Feeds are kept per scope so a tenant's stream can never carry another
// tenant's job names or progress.
func (s *Server) jobFeed(scope models.Scope) *feed {
	key := "admin"
	if !scope.Admin {
		key = "user:" + strconv.FormatInt(scope.UserID, 10)
	}

	s.feedMu.Lock()
	defer s.feedMu.Unlock()
	if s.jobFeeds == nil {
		s.jobFeeds = map[string]*feed{}
	}
	if f, ok := s.jobFeeds[key]; ok {
		return f
	}

	f := newFeed(2*time.Second, func() (string, string, error) {
		return s.renderJobTracker(scope)
	})
	s.jobFeeds[key] = f
	return f
}

// renderJobTracker renders the job_list partial and returns its HTML plus a
// fingerprint used to detect changes between polls.
func (s *Server) renderJobTracker(scope models.Scope) (string, string, error) {
	jobs, err := s.App.DB.ListActiveJobsScoped(scope)
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

	list, err := s.Instances.List(r.Context(), scopeFrom(r))
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

	list, err := s.Instances.List(r.Context(), scopeFrom(r))
	if err != nil {
		s.renderError(w, r, "Networking", "networking", err)
		return
	}

	data := networkingData{
		baseData:     s.newBase(w, r, "networking", "Networking"),
		Networks:     networks,
		Instances:    list,
		CaddyEnabled: s.Domains.CaddyEnabled(),
	}
	if checkAdminAccess(r) == nil {
		data.CaddyPath = s.App.Cfg.CaddyConfigPath
		data.CaddyAdmin = s.App.Cfg.CaddyAdminURL
	}
	s.render(w, r, "networking", http.StatusOK, data)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, nil)
}

// renderSettings draws the Settings page. recoveryCodes is non-nil only in the
// response to generating them, the one time they can be read.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, recoveryCodes []string) {
	data := settingsData{
		baseData: s.newBase(w, r, "settings", "Settings"),
	}

	// Everyone gets their own security settings. Host, user, provider, storage
	// and routing details are for admins only, so they are never even loaded
	// for anyone else.
	if checkAdminAccess(r) == nil {
		if err := s.fillAdminSettings(r, &data); err != nil {
			s.renderError(w, r, "Settings", "settings", err)
			return
		}
	}

	if me := userFrom(r); me != nil {
		data.Sessions = s.sessionViews(r, me.ID)
		if enabled, err := s.SSO.PublicProviders(); err == nil {
			data.LinkProviders = enabled
			data.MyLinks = s.linkedProviders(me.ID)
		}
	}

	// Two-factor state for the signed-in user. A pending setup secret is
	// shown once, right after generation.
	if me := userFrom(r); me != nil {
		if fresh, err := s.App.DB.GetUser(me.ID); err == nil {
			data.TOTPEnabled = fresh.TOTPEnabled
			data.RecoveryCodes = recoveryCodes
			if fresh.TOTPEnabled {
				data.RecoveryLeft = s.App.Auth.RecoveryCodesLeft(fresh.ID)
			}
			if r.URL.Query().Get("totp") == "setup" && !fresh.TOTPEnabled && fresh.TOTPSecret != "" {
				data.TOTPSetupSecret = fresh.TOTPSecret
				data.TOTPSetupURL = auth.ProvisioningURL(fresh.Username, fresh.TOTPSecret)
				data.TOTPSetupQR = totpQRCode(data.TOTPSetupURL)
			}
		}
	}

	s.render(w, r, "settings", http.StatusOK, data)
}

// fillAdminSettings loads the settings that only administrators may see.
func (s *Server) fillAdminSettings(r *http.Request, data *settingsData) error {
	host, err := s.Monitoring.HostStats()
	if err != nil {
		s.log.Warn("could not read host stats", "err", err)
		host = &monitoringHostFallback
	}

	users, err := s.App.DB.ListUsers()
	if err != nil {
		return err
	}

	sessions, err := s.App.DB.CountSessions()
	if err != nil {
		s.log.Warn("could not count sessions", "err", err)
	}

	data.Host = host
	data.Users = users
	data.Links = s.SSO.IdentityLabels()
	if usage, err := s.App.DB.InstanceCountsByOwner(); err != nil {
		s.log.Warn("could not count instances per owner", "err", err)
	} else {
		data.Usage = usage
	}

	data.S3Configured = s.App.Cfg.S3Configured()
	data.S3Bucket = s.App.Cfg.S3Bucket
	data.S3Endpoint = s.App.Cfg.S3Endpoint
	data.S3Region = s.App.Cfg.S3Region
	data.S3Prefix = s.App.Cfg.S3Prefix
	data.S3AccessKeyMasked = maskSecret(s.App.Cfg.S3AccessKey)

	data.CaddyEnabled = s.Domains.CaddyEnabled()
	data.CaddyPath = s.App.Cfg.CaddyConfigPath
	data.CaddyAdmin = s.App.Cfg.CaddyAdminURL
	data.CaddyReloadCmd = s.App.Cfg.CaddyReloadCmd
	data.CaddyPreview = s.Domains.PreviewCaddyfile(r.Context())

	data.SessionCount = sessions
	data.MetricsInterval = s.App.Cfg.MetricsIntervalSeconds
	data.MetricsRetention = s.App.Cfg.MetricsRetentionHours
	data.JobWorkers = jobWorkerCount
	data.IncusProject = s.App.Cfg.IncusProject

	if providers, err := s.SSO.ProviderViews(); err != nil {
		s.log.Warn("could not list sso providers", "err", err)
	} else {
		data.Providers = providers
	}
	data.SSOKeySet = s.App.Cfg.SSOKeySet()

	if list, err := s.Instances.List(r.Context(), scopeFrom(r)); err == nil {
		running := 0
		for _, i := range list {
			if i.Running {
				running++
			}
		}
		data.InstanceCount = len(list)
		data.RunningCount = running
	}
	return nil
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

	quota := 0
	var quotaErr error
	if raw := strings.TrimSpace(r.FormValue("max_instances")); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			quotaErr = errors.New("the instance quota must be zero or a positive whole number")
		} else {
			quota = n
		}
	}

	if err := checkAdminAccess(r); err != nil {
		s.App.Activity.Record(actor.Username, "Create user", username, role, err)
		s.finish(w, r, "/settings", err, "")
		return
	}
	if quotaErr != nil {
		s.App.Activity.Record(actor.Username, "Create user", username, role, quotaErr)
		s.finish(w, r, "/settings", quotaErr, "")
		return
	}

	err := s.createUser(username, password, role, quota)
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

	// A refused request still re-renders the list, which must stay empty
	// for anyone who may not see the users.
	if checkAdminAccess(r) == nil {
		data.Links = s.SSO.IdentityLabels()
		users, err := s.App.DB.ListUsers()
		if err != nil {
			data.Error = friendlyError(err)
		} else {
			data.Users = users
		}
		if usage, err := s.App.DB.InstanceCountsByOwner(); err != nil {
			s.log.Warn("could not count instances per owner", "err", err)
		} else {
			data.Usage = usage
		}
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
		} else if berr := s.userRemovalBlocked(target.ID); berr != nil {
			err = berr
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

// userRemovalBlocked refuses deleting an account that still owns instances or
// has work in flight. Deleting it would orphan the instances (they become
// admin-only) or leave a queued job pointing at a tenant that no longer exists.
func (s *Server) userRemovalBlocked(id int64) error {
	if n, err := s.App.DB.CountInstancesByOwner(id); err != nil {
		return err
	} else if n > 0 {
		return fmt.Errorf("this user owns %d instance(s) — reassign them before deleting the account", n)
	}

	if n, err := s.App.DB.CountActiveJobsByOwner(id); err != nil {
		return err
	} else if n > 0 {
		return fmt.Errorf("this user has %d queued or running job(s) — wait for them to finish", n)
	}
	return nil
}

// handleUserQuota changes a user's per-account instance limit. Only admins may
// do so; 0 means unlimited and the host-wide limit still applies.
func (s *Server) handleUserQuota(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.PathValue("id"), 0))
	raw := strings.TrimSpace(r.FormValue("max_instances"))

	var target *models.User
	var quota int
	err := checkAdminAccess(r)
	if err == nil {
		target, err = s.App.DB.GetUser(id)
	}
	if err == nil {
		if quota, err = strconv.Atoi(raw); err != nil || quota < 0 {
			quota = 0
			err = errors.New("the instance quota must be zero or a positive whole number")
		}
	}
	if err == nil {
		err = s.App.DB.SetUserQuota(id, quota)
	}

	label := ""
	if target != nil {
		label = target.Username
	}
	detail := ""
	notice := ""
	if err != nil {
		s.App.Activity.Record(actor.Username, "Set instance quota", label, raw, err)
	} else {
		detail = fmt.Sprintf("max_instances=%d", quota)
		notice = "Quota for " + label + " set to " + strconv.Itoa(quota) + "."
		s.App.Activity.Record(actor.Username, "Set instance quota", label, detail, nil)
	}

	if isHTMX(r) {
		s.serveUsersFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, notice)
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

	fresh, err := s.App.DB.GetUser(user.ID)
	if err != nil {
		s.totpFailure(w, r, err)
		return
	}
	// Replacing the secret switches enforcement off, so on an account that
	// already has two-factor it would be a password-free way to disable it.
	if fresh.TOTPEnabled {
		s.totpFailure(w, r, errors.New("two-factor is already on — turn it off first to set it up again"))
		return
	}
	if err := s.confirmPassword(fresh, r); err != nil {
		s.App.Activity.Record(user.Username, "Start two-factor setup", user.Username, "", err)
		s.totpFailure(w, r, err)
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
		// Codes are made here so nobody turns 2FA on without a way back in.
		codes, cerr := s.App.Auth.GenerateRecoveryCodes(user.ID)
		if cerr != nil {
			s.log.Error("could not generate recovery codes", "err", cerr)
		}
		s.serveTOTPFragmentsWithCodes(w, r, nil, "Two-factor authentication is on.", codes)
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
	if err := s.confirmPassword(fresh, r); err != nil {
		s.App.Activity.Record(user.Username, "Disable two-factor", user.Username, "", err)
		s.totpFailure(w, r, err)
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

// handleTOTPRecovery replaces the user's recovery codes after confirming the
// password, and shows the new set once.
func (s *Server) handleTOTPRecovery(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	fresh, err := s.App.DB.GetUser(user.ID)
	if err != nil {
		s.totpFailure(w, r, err)
		return
	}
	if !fresh.TOTPEnabled {
		s.totpFailure(w, r, errors.New("turn on two-factor first"))
		return
	}
	if err := s.confirmPassword(fresh, r); err != nil {
		s.App.Activity.Record(user.Username, "New recovery codes", user.Username, "", err)
		s.totpFailure(w, r, err)
		return
	}

	codes, err := s.App.Auth.GenerateRecoveryCodes(user.ID)
	s.App.Activity.Record(user.Username, "New recovery codes", user.Username, "", err)
	if err != nil {
		s.totpFailure(w, r, err)
		return
	}

	if isHTMX(r) {
		s.serveTOTPFragmentsWithCodes(w, r, nil, "New recovery codes generated. The old ones no longer work.", codes)
		return
	}
	s.renderSettings(w, r, codes)
}

// confirmPassword asks the signed-in user to prove they know their password
// before a security setting changes. Accounts that cannot sign in with a
// password have nothing to confirm, and two-factor never applies to them.
func (s *Server) confirmPassword(user *models.User, r *http.Request) error {
	if !sso.PasswordAllowed(user.AuthMethod) {
		return nil
	}
	err := s.App.Auth.ConfirmPassword(user.ID, r.FormValue("current_password"))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, auth.ErrInvalidCredentials):
		return errors.New("incorrect password")
	default:
		return err
	}
}

// totpFailure reports a failed two-factor change in place for HTMX, or by
// redirect otherwise.
func (s *Server) totpFailure(w http.ResponseWriter, r *http.Request, err error) {
	if isHTMX(r) {
		s.serveTOTPFragments(w, r, err, "")
		return
	}
	s.fail(w, r, "/settings", err)
}

// serveTOTPFragments renders the HTMX response for two-factor operations:
// the fresh TOTP card plus an out-of-band flash. A pending (unconfirmed)
// secret is shown again so a mistyped code can simply be retried.
func (s *Server) serveTOTPFragments(w http.ResponseWriter, r *http.Request, actionErr error, notice string) {
	s.serveTOTPFragmentsWithCodes(w, r, actionErr, notice, nil)
}

// serveTOTPFragmentsWithCodes is serveTOTPFragments that also shows freshly
// generated recovery codes.
func (s *Server) serveTOTPFragmentsWithCodes(w http.ResponseWriter, r *http.Request, actionErr error, notice string, codes []string) {
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
			data.RecoveryCodes = codes
			if fresh.TOTPEnabled {
				data.RecoveryLeft = s.App.Auth.RecoveryCodesLeft(fresh.ID)
			}
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
func (s *Server) createUser(username, password, role string, maxInstances int) error {
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

	if maxInstances < 0 {
		return errors.New("the instance quota must be zero or a positive whole number")
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

	user, err := s.App.DB.CreateUser(username, hash, role)
	if err != nil {
		return err
	}
	if maxInstances > 0 {
		return s.App.DB.SetUserQuota(user.ID, maxInstances)
	}
	return nil
}
