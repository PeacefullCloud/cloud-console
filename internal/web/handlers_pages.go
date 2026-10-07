package web

import (
	"errors"
	"net/http"
	"strings"

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

	err := s.Domains.Add(r.Context(), name, domain, port)
	s.App.Activity.Record(username, "Add domain", name, domain, err)

	s.finish(w, r, s.InstanceURL(name)+"?tab=domains", err, "Added "+domain+" to "+name+".")
}

func (s *Server) handleInstanceDomainDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id := int64(atoiDefault(r.PathValue("id"), 0))
	username := usernameOf(userFrom(r))

	err := s.Domains.Delete(r.Context(), id)
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

	err := s.Domains.Delete(r.Context(), id)
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

	err := s.createUser(username, password, role)
	s.App.Activity.Record(actor.Username, "Create user", username, role, err)

	s.finish(w, r, "/settings", err, "User "+username+" created.")
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
		s.fail(w, r, "/settings", err)
		return
	}

	// Changing the password invalidates every session, including this one.
	s.clearCookie(w, sessionCookie)
	s.setFlash(w, "ok", "Password changed. Please sign in again.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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
