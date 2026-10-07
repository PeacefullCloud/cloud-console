package web

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/peaceful/cloud-console/internal/backups"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/instances"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/snapshots"
)

// --- dashboard ------------------------------------------------------------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	list, err := s.Instances.List(r.Context())
	if err != nil {
		s.renderError(w, r, "Getting started", "getting-started", err)
		return
	}

	totals, err := s.Instances.Totals(r.Context(), list)
	if err != nil {
		s.log.Warn("could not compute totals", "err", err)
	}

	host, err := s.Monitoring.HostStats()
	if err != nil {
		s.log.Warn("could not read host stats", "err", err)
		host = &monitoringHostFallback
	}

	activity, err := s.App.DB.ListActivity(8, 0)
	if err != nil {
		s.log.Warn("could not read activity", "err", err)
	}

	data := dashboardData{
		baseData:  s.withJobs(s.newBase(w, r, "getting-started", "Getting started")),
		Instances: list,
		Totals:    totals,
		Host:      host,
		Activity:  activity,
		S3Enabled: s.Backups.S3Configured(),
	}
	// The navigation badge follows the live list, not just tracked instances.
	data.InstanceCount = len(list)

	s.render(w, r, "dashboard", http.StatusOK, data)
}

// --- instances list -------------------------------------------------------

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	list, err := s.Instances.List(r.Context())
	if err != nil {
		s.renderError(w, r, "Instances", "instances", err)
		return
	}

	if query != "" {
		list = filterInstances(list, query)
	}

	running, stopped := instances.CountByStatus(list)

	data := instancesData{
		baseData:  s.withJobs(s.newBase(w, r, "instances", "Instances")),
		Instances: list,
		Groups:    s.groupByPool(list),
		Running:   running,
		Stopped:   stopped,
	}
	data.InstanceCount = len(list)

	s.render(w, r, "instances", http.StatusOK, data)
}

// filterInstances matches on name, image label, owner or domain.
func filterInstances(list []instances.Instance, query string) []instances.Instance {
	q := strings.ToLower(query)

	out := make([]instances.Instance, 0, len(list))
	for _, inst := range list {
		haystack := strings.ToLower(inst.Name + " " + inst.ImageLabel + " " + inst.Image + " " +
			inst.Owner + " " + inst.PrimaryHost + " " + inst.Status + " " + inst.Kind)
		for _, d := range inst.Domains {
			haystack += " " + strings.ToLower(d.Domain)
		}
		if strings.Contains(haystack, q) {
			out = append(out, inst)
		}
	}
	return out
}

// groupByPool arranges instances under their storage pool, mirroring the
// Lightsail region/zone grouping.
func (s *Server) groupByPool(list []instances.Instance) []poolGroup {
	index := map[string]int{}
	var groups []poolGroup
	drivers := s.poolDrivers()

	for _, inst := range list {
		name := inst.StoragePool
		pos, ok := index[name]
		if !ok {
			groups = append(groups, poolGroup{Name: name, Driver: drivers[name]})
			pos = len(groups) - 1
			index[name] = pos
		}
		groups[pos].Instances = append(groups[pos].Instances, inst)
	}

	sort.Slice(groups, func(i, j int) bool {
		// Named pools first, unassigned last.
		if groups[i].Name == "" {
			return false
		}
		if groups[j].Name == "" {
			return true
		}
		return groups[i].Name < groups[j].Name
	})

	return groups
}

// --- create wizard --------------------------------------------------------

func (s *Server) handleCreatePage(w http.ResponseWriter, r *http.Request) {
	data, err := s.createData(w, r)
	if err != nil {
		s.renderError(w, r, "Create an instance", "instances", err)
		return
	}
	s.render(w, r, "create", http.StatusOK, data)
}

// handleBlueprints re-renders the blueprint grid when the platform changes.
func (s *Server) handleBlueprints(w http.ResponseWriter, r *http.Request) {
	data, err := s.createData(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.Renderer.RenderPartial(w, "create", "blueprint_grid", data); err != nil {
		s.log.Error("render blueprints", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// createData assembles the wizard state, honouring query parameters so the
// platform and image selections survive an HTMX re-render.
func (s *Server) createData(w http.ResponseWriter, r *http.Request) (createData, error) {
	defaultImage := "ubuntu-24.04"
	platform := r.URL.Query().Get("platform")
	selectedImage := r.URL.Query().Get("image")

	if platform == "" {
		platform = "os"
	}

	catalog := incus.DefaultCatalog()

	images := make([]incus.Image, 0, len(catalog))
	for _, img := range catalog {
		if platform == "apps" {
			if img.Category != "app" {
				continue
			}
		} else if img.Category != "os" {
			continue
		}
		images = append(images, img)
	}

	if len(images) > 0 {
		defaultImage = images[0].ID
	}
	if selectedImage == "" {
		selectedImage = defaultImage
	}

	pools := []poolInfo{}
	if raw, err := s.Instances.StoragePools(); err == nil {
		for _, p := range raw {
			pools = append(pools, poolInfo{Name: p.Name, Driver: p.Driver})
		}
	}

	defaultPool := s.Instances.DefaultStoragePool()

	sizes := DefaultSizes()

	data := createData{
		baseData:     s.newBase(w, r, "instances", "Create an instance"),
		Platforms:    platformInfos(catalog),
		Images:       images,
		Sizes:        sizes,
		Pools:        pools,
		DefaultImage: defaultImage,
		Form: createForm{
			Platform:    platform,
			Image:       selectedImage,
			Kind:        instances.KindContainer,
			Size:        sizes[2].Value(),
			StoragePool: defaultPool,
			CPU:         sizes[2].CPU,
			MemoryMB:    sizes[2].MemoryMB,
			DiskGB:      sizes[2].DiskGB,
		},
	}

	return data, nil
}

func platformInfos(catalog []incus.Image) []platformInfo {
	var osCount, appCount int
	for _, img := range catalog {
		if img.Category == "app" {
			appCount++
		} else {
			osCount++
		}
	}

	return []platformInfo{
		{
			ID: "apps", Label: "Linux apps", Count: appCount,
			Description: "Pre-configured applications such as WordPress with Caddy, PHP-FPM and MariaDB.",
		},
		{
			ID: "os", Label: "Linux operating system", Count: osCount,
			Description: "Start from a clean Ubuntu, Debian, AlmaLinux or Alpine image.",
		},
	}
}

func (s *Server) handleCreateSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, "/create", err)
		return
	}

	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	req := instances.CreateRequest{
		Name:        strings.ToLower(strings.TrimSpace(r.FormValue("name"))),
		ImageID:     r.FormValue("image"),
		Kind:        strings.TrimSpace(r.FormValue("kind")),
		Domain:      strings.TrimSpace(r.FormValue("domain")),
		Notes:       strings.TrimSpace(r.FormValue("notes")),
		StoragePool: r.FormValue("storage_pool"),
		OwnerID:     user.ID,
		OwnerName:   user.Username,
	}
	if req.Kind == "" {
		req.Kind = instances.KindContainer
	}

	// A preset plan carries its own resources; "custom" uses the number fields.
	if sizeValue := r.FormValue("size"); sizeValue != "" && sizeValue != "custom" {
		cpu, memMB, diskGB, ok := ParseSizeValue(sizeValue)
		if !ok {
			s.fail(w, r, "/create", errors.New("could not read the selected plan"))
			return
		}
		req.CPU, req.MemoryMB, req.DiskGB = cpu, memMB, diskGB
	} else {
		req.CPU = atoiDefault(r.FormValue("cpu"), 2)
		req.MemoryMB = atoiDefault(r.FormValue("memory_mb"), 2048)
		req.DiskGB = atoiDefault(r.FormValue("disk_gb"), 40)
	}

	if err := req.Validate(); err != nil {
		s.App.Activity.Record(user.Username, "Create instance", req.Name, "", err)
		s.fail(w, r, "/create", err)
		return
	}

	job, err := s.Instances.EnqueueCreate(user.Username, req)
	if err != nil {
		s.App.Activity.Record(user.Username, "Create instance", req.Name, "", err)
		s.fail(w, r, "/create", err)
		return
	}

	s.App.Activity.Record(user.Username, "Create instance", req.Name,
		"queued as job "+job.ID, nil)

	// Always land on /instances where the job tracker lives. Do not use
	// redirectBack here: the Referer is /create, which would leave the user
	// on the wizard with no progress feedback.
	s.setFlash(w, "ok", "Creating "+req.Name+" — this takes a moment.")
	http.Redirect(w, r, "/instances", http.StatusSeeOther)
}

// --- instance detail ------------------------------------------------------

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tab := normaliseTab(r.URL.Query().Get("tab"))
	window := normaliseWindow(r.URL.Query().Get("window"))

	data, err := s.instanceData(w, r, name, tab, window)
	if err != nil {
		if errors.Is(err, instances.ErrNotFound) || incus.IsNotFound(err) {
			s.setFlash(w, "err", "Instance "+name+" was not found.")
			http.Redirect(w, r, "/instances", http.StatusSeeOther)
			return
		}
		s.renderError(w, r, name, "instances", err)
		return
	}

	s.render(w, r, "instance", http.StatusOK, data)
}

// handleInstanceTab renders a single tab panel for HTMX navigation.
func (s *Server) handleInstanceTab(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tab := normaliseTab(r.PathValue("tab"))
	window := normaliseWindow(r.URL.Query().Get("window"))

	data, err := s.instanceData(w, r, name, tab, window)
	if err != nil {
		http.Error(w, friendlyError(err), http.StatusNotFound)
		return
	}

	if err := s.Renderer.RenderPartial(w, "instance", "tab_"+tab, data); err != nil {
		s.log.Error("render instance tab", "err", err, "tab", tab)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Server) instanceData(w http.ResponseWriter, r *http.Request, name, tab, window string) (instanceData, error) {
	inst, err := s.Instances.Get(r.Context(), name)
	if err != nil {
		return instanceData{}, err
	}

	windowDuration, windowLabel := windowDuration(window)

	data := instanceData{
		baseData:          s.withJobs(s.newBase(w, r, "instances", name)),
		Inst:              inst,
		Tab:               tab,
		Tabs:              instanceTabs(),
		Window:            window,
		Windows:           windowOptions(window),
		WindowLabel:       windowLabel,
		SampleInterval:    s.App.Cfg.MetricsIntervalSeconds,
		Images:            incus.DefaultCatalog(),
		Sizes:             DefaultSizes(),
		S3Enabled:         s.Backups.S3Configured(),
		S3Bucket:          s.Backups.S3Bucket(),
		CaddyEnabled:      s.Domains.CaddyEnabled(),
		SuggestedSnapshot: snapshots.SuggestName(timeNow()),
		SuggestedBackup:   backups.SuggestName(name, timeNow()),
	}

	switch tab {
	case "metrics":
		samples, err := s.Monitoring.History(name, windowDuration)
		if err != nil {
			s.log.Warn("could not read metrics", "instance", name, "err", err)
		}
		data.CPUSeries = monitoringPercentSeries(samples)
		data.MemSeries = monitoringMemorySeries(samples)

	case "snapshots":
		list, err := s.Snapshots.List(name)
		if err != nil {
			s.log.Warn("could not list snapshots", "instance", name, "err", err)
		}
		data.Snapshots = list

	case "backups":
		list, err := s.Backups.List(r.Context(), name)
		if err != nil {
			s.log.Warn("could not list backups", "instance", name, "err", err)
		}
		data.Backups = list

	case "domains", "networking":
		list, err := s.Domains.ListForInstance(r.Context(), name)
		if err != nil {
			s.log.Warn("could not list domains", "instance", name, "err", err)
		}
		data.Domains = list

	case "storage":
		data.Disks = s.attachedDisks(name)

	case "history":
		activity, err := s.App.DB.ListActivityForInstance(name, 100)
		if err != nil {
			s.log.Warn("could not read history", "instance", name, "err", err)
		}
		data.Activity = activity
	}

	if tab == "overview" || tab == "history" {
		if activity, err := s.App.DB.ListActivityForInstance(name, 6); err == nil && len(data.Activity) == 0 {
			data.Activity = activity
		}
	}

	if tab == "overview" {
		data.ConsoleLog, data.ConsoleLogErr = s.consoleTail(name, 40)
	}

	if tab == "networking" {
		data.Networks = s.networkDevices(name)
	}

	return data, nil
}

func normaliseTab(tab string) string {
	switch tab {
	case "overview", "metrics", "snapshots", "storage", "networking", "domains", "backups", "history", "settings":
		return tab
	default:
		return "overview"
	}
}

func normaliseWindow(window string) string {
	switch window {
	case "hour", "six", "day", "week", "month":
		return window
	default:
		return "hour"
	}
}

// --- lifecycle actions ----------------------------------------------------

func (s *Server) handleInstanceState(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	action := r.FormValue("action")
	user := userFrom(r)
	username := usernameOf(user)

	ctx := r.Context()
	var err error

	switch action {
	case "start":
		err = s.Instances.Start(ctx, name)
	case "stop":
		err = s.Instances.Stop(ctx, name)
	case "restart":
		err = s.Instances.Restart(ctx, name)
	default:
		err = errors.New("unknown action " + action)
	}

	s.App.Activity.Record(username, titleAction(action), name, "", err)
	s.finish(w, r, s.InstanceURL(name), err, titleAction(action)+" requested for "+name+".")
}

func (s *Server) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	username := usernameOf(userFrom(r))

	err := s.Instances.Delete(r.Context(), name)
	s.App.Activity.Record(username, "Delete instance", name, "", err)

	if err == nil {
		s.Monitoring.Forget(name)
	}
	s.finish(w, r, "/instances", err, "Deleted "+name+".")
}

func (s *Server) handleInstanceRename(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	newName := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	username := usernameOf(userFrom(r))

	err := s.Instances.Rename(r.Context(), name, newName)
	s.App.Activity.Record(username, "Rename instance", name, "to "+newName, err)

	if err != nil {
		s.finish(w, r, s.InstanceURL(name), err, "")
		return
	}

	s.setFlash(w, "ok", "Renamed "+name+" to "+newName+".")
	http.Redirect(w, r, s.InstanceURL(newName)+"?tab=settings", http.StatusSeeOther)
}

func (s *Server) handleInstanceRebuild(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	image := r.FormValue("image")
	username := usernameOf(userFrom(r))

	job, err := s.Instances.EnqueueRebuild(username, name, image)
	s.App.Activity.Record(username, "Rebuild instance", name, "from "+image, err)

	if err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=settings", err, "")
		return
	}

	s.setFlash(w, "ok", "Rebuilding "+name+" (job "+job.ID+").")
	http.Redirect(w, r, s.InstanceURL(name), http.StatusSeeOther)
}

func (s *Server) handleInstanceLimits(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	username := usernameOf(userFrom(r))

	cpu := atoiDefault(r.FormValue("cpu"), 0)
	memoryMB := atoiDefault(r.FormValue("memory_mb"), 0)
	diskGB := atoiDefault(r.FormValue("disk_gb"), 0)

	err := s.Instances.UpdateLimits(r.Context(), name, cpu, memoryMB, diskGB)
	s.App.Activity.Record(username, "Update limits", name,
		"cpu="+strconv.Itoa(cpu)+" mem="+strconv.Itoa(memoryMB)+"MB disk="+strconv.Itoa(diskGB)+"GB", err)

	s.finish(w, r, s.InstanceURL(name)+"?tab=settings", err, "Resource limits updated.")
}

func (s *Server) handleInstanceNotes(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	meta, err := s.App.DB.GetInstanceMeta(name)
	if err != nil {
		s.fail(w, r, s.InstanceURL(name)+"?tab=settings", err)
		return
	}

	meta.Notes = strings.TrimSpace(r.FormValue("notes"))
	err = s.App.DB.SaveInstanceMeta(meta)
	if err != nil {
		s.fail(w, r, s.InstanceURL(name)+"?tab=settings", err)
		return
	}

	s.App.Activity.Record(usernameOf(userFrom(r)), "Update notes", name, "", nil)
	s.succeed(w, r, s.InstanceURL(name)+"?tab=settings", "Notes saved.")
}

// InstanceURL builds the canonical instance page path.
func (s *Server) InstanceURL(name string) string { return "/instances/" + name }

// --- errors ---------------------------------------------------------------

// renderError shows a friendly failure page.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, title, nav string, err error) {
	s.log.Error("page error", "title", title, "path", r.URL.Path, "err", err)

	base := s.newBase(w, r, nav, title)
	base.Error = friendlyError(err)

	// Reuse the dashboard shell so the user always has navigation.
	data := dashboardData{baseData: base, Host: &monitoringHostFallback}
	if renderErr := s.Renderer.Render(w, http.StatusOK, "dashboard", data); renderErr != nil {
		http.Error(w, friendlyError(err), http.StatusInternalServerError)
	}
}

// render writes a page and logs template failures.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, data any) {
	if err := s.Renderer.Render(w, status, page, data); err != nil {
		s.log.Error("render failed", "page", page, "err", err)
		if status == http.StatusOK {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		}
	}
}

// finish reports the outcome of a form action and redirects.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, fallback string, err error, okMessage string) {
	if err != nil {
		s.fail(w, r, fallback, err)
		return
	}
	s.succeed(w, r, fallback, okMessage)
}

func titleAction(action string) string {
	switch action {
	case "start":
		return "Start instance"
	case "stop":
		return "Stop instance"
	case "restart":
		return "Reboot instance"
	default:
		if action == "" {
			return "Instance action"
		}
		return strings.ToUpper(action[:1]) + action[1:]
	}
}

func usernameOf(user *models.User) string {
	if user == nil {
		return ""
	}
	return user.Username
}
