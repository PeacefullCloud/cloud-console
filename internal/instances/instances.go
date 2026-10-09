// Package instances implements the console's instance management.
//
// It merges two sources: Incus (live infrastructure truth) and SQLite (console
// bookkeeping such as the friendly image label, owner and primary hostname).
package instances

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// Errors surfaced to the UI.
var (
	ErrNotFound     = errors.New("instance not found")
	ErrInvalidName  = errors.New("invalid instance name")
	ErrInvalidSpec  = errors.New("invalid resource specification")
	ErrNameInUse    = errors.New("an instance with that name already exists")
	ErrLimitsTooBig = errors.New("requested resources exceed host capacity")
	ErrQuota        = errors.New("instance limit reached")
	ErrBusy         = errors.New("a background job is still running for this instance — wait for it to finish")
)

// namePattern matches DNS-safe Incus instance names.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Notifier is told when an instance changes so Caddy can be re-rendered.
type Notifier interface {
	InstanceChanged(ctx context.Context, name string)
}

// ArchiveKeeper owns data kept outside Incus that is filed under an instance
// name (the backups service). It is told when that name changes or goes away.
type ArchiveKeeper interface {
	InstanceRenamed(oldName, newName string)
	InstanceDeleted(ctx context.Context, name string)
}

// Service manages instances.
type Service struct {
	app      *core.App
	notifier Notifier
	archives ArchiveKeeper

	imageLabels imageLabelCache

	createMu sync.Mutex
}

// New creates the instance service.
func New(app *core.App) *Service {
	return &Service{app: app}
}

// SetNotifier installs the change hook (the domains service).
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// SetArchiveKeeper installs the hook that keeps backup archives in step with
// renames and deletes.
func (s *Service) SetArchiveKeeper(k ArchiveKeeper) { s.archives = k }

// Instance is the view model used by templates.
type Instance struct {
	Name    string
	Kind    string // container | virtual-machine
	Status  string // Running | Stopped | ...
	Running bool
	IP      string
	IPs     []string

	Image      string // Incus image alias
	ImageLabel string // friendly name

	CPU         int
	MemoryBytes int64
	DiskBytes   int64
	StoragePool string

	CreatedAt   time.Time
	LastUsedAt  time.Time
	Owner       string
	PrimaryHost string
	Notes       string

	Domains []models.Domain

	// Live usage, read from the monitor's latest sample when available.
	CPUPercent  float64
	MemUsed     int64
	DiskUsed    int64
	NetRx       int64
	NetTx       int64
	Processes   int64
	StartedAt   time.Time
	Tracked     bool // known to the console (created through it)
	Description string
}

// Uptime returns how long the instance has been running.
func (i Instance) Uptime() time.Duration {
	if i.StartedAt.IsZero() || !i.Running {
		return 0
	}
	return time.Since(i.StartedAt)
}

// MemoryGB renders the memory limit in whole gigabytes for compact UI.
func (i Instance) MemoryGB() string {
	if i.MemoryBytes <= 0 {
		return "unlimited"
	}
	return incus.FormatGB(i.MemoryBytes)
}

// DiskLabel renders the root disk size.
func (i Instance) DiskLabel() string {
	if i.DiskBytes <= 0 {
		return "pool default"
	}
	return incus.FormatGB(i.DiskBytes)
}

// CPULabel renders the vCPU count.
func (i Instance) CPULabel() string {
	if i.CPU <= 0 {
		return "shared"
	}
	return fmt.Sprintf("%d vCPU", i.CPU)
}

// List returns every instance known to Incus, enriched with console metadata.
func (s *Service) List(ctx context.Context) ([]Instance, error) {
	fulls, err := s.app.Incus.Instances(ctx)
	if err != nil {
		return nil, err
	}

	meta, err := s.app.DB.ListInstanceMeta()
	if err != nil {
		return nil, err
	}

	domains, err := s.app.DB.ListDomainsByInstance()
	if err != nil {
		return nil, err
	}

	samples, err := s.app.DB.LatestMetricsMap()
	if err != nil {
		s.app.Log.Warn("could not read latest metrics", "err", err)
		samples = map[string]models.Metric{}
	}

	out := make([]Instance, 0, len(fulls))
	for i := range fulls {
		out = append(out, s.merge(&fulls[i], meta, domains, samples))
	}

	// Stable, human-friendly ordering.
	sortInstances(out)
	return out, nil
}

// Get returns a single instance view.
func (s *Service) Get(ctx context.Context, name string) (*Instance, error) {
	full, err := s.app.Incus.InstanceFull(name)
	if err != nil {
		if incus.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	m := map[string]models.InstanceMeta{}
	if meta, err := s.app.DB.GetInstanceMeta(name); err == nil {
		m[name] = *meta
	}

	domains, err := s.app.DB.ListDomainsForInstance(name)
	if err != nil {
		return nil, err
	}

	samples, err := s.app.DB.LatestMetricsMap()
	if err != nil {
		samples = map[string]models.Metric{}
	}

	view := s.merge(full, m, map[string][]models.Domain{name: domains}, samples)
	return &view, nil
}

// RawInstance returns the Incus record, used by handlers that need config or
// devices.
func (s *Service) RawInstance(name string) (*incusapi.Instance, error) {
	return s.app.Incus.Instance(name)
}

// merge combines an Incus record with console metadata.
func (s *Service) merge(full *incusapi.InstanceFull, meta map[string]models.InstanceMeta,
	domains map[string][]models.Domain, samples map[string]models.Metric) Instance {

	inst := full.Instance

	view := Instance{
		Name:        inst.Name,
		Kind:        incus.InstanceKind(inst),
		Status:      inst.Status,
		Running:     inst.StatusCode == incusapi.Running,
		IPs:         incus.InstanceIPs(full),
		CPU:         incus.InstanceCPU(inst),
		MemoryBytes: incus.InstanceMemoryBytes(inst),
		DiskBytes:   incus.InstanceDiskBytes(inst),
		StoragePool: incus.InstanceStoragePool(inst),
		CreatedAt:   inst.CreatedAt,
		LastUsedAt:  inst.LastUsedAt,
		Description: inst.Description,
	}

	view.IP = incus.InstanceIP(full)
	if view.IP == "" && len(view.IPs) > 0 {
		view.IP = view.IPs[0]
	}

	for _, d := range domains[inst.Name] {
		if view.PrimaryHost == "" {
			view.PrimaryHost = d.Domain
		}
	}

	if m, ok := meta[inst.Name]; ok {
		view.Tracked = true
		view.Image = m.Image
		view.ImageLabel = m.ImageLabel
		view.Owner = m.OwnerName
		view.Notes = m.Notes
		if m.PrimaryHost != "" {
			view.PrimaryHost = m.PrimaryHost
		}
		// Prefer recorded limits when Incus reports none.
		if view.CPU == 0 && m.CPU > 0 {
			view.CPU = m.CPU
		}
		if view.MemoryBytes == 0 && m.MemoryMB > 0 {
			view.MemoryBytes = int64(m.MemoryMB) << 20
		}
		if view.DiskBytes == 0 && m.DiskGB > 0 {
			view.DiskBytes = int64(m.DiskGB) << 30
		}
		if view.StoragePool == "" {
			view.StoragePool = m.StoragePool
		}
	}

	// Fall back to the image fingerprint when the alias is unknown, so a card
	// never shows an empty name.
	if view.ImageLabel == "" {
		if alias, ok := inst.Config["image.os"]; ok && alias != "" {
			view.ImageLabel = alias
		} else {
			view.ImageLabel = s.imageLabels.get(inst.Config["volatile.base_image"], s.lookupImageLabel)
		}
	}

	view.Domains = domains[inst.Name]

	if sample, ok := samples[inst.Name]; ok {
		view.CPUPercent = sample.CPUPct
		view.MemUsed = sample.MemUsed
		view.DiskUsed = sample.DiskUsed
		view.NetRx = sample.NetRxBytes
		view.NetTx = sample.NetTxBytes
	} else if full.State != nil {
		// No history yet: fall back to the raw live counters.
		view.MemUsed = incus.StateMemoryUsed(full.State)
		view.DiskUsed = incus.StateDiskUsed(full.State)
		view.NetRx, view.NetTx = incus.StateNetworkTotals(full.State)
	}

	if full.State != nil {
		view.Processes = full.State.Processes
		view.StartedAt = full.State.StartedAt
	}

	return view
}

// ValidateName checks an instance name against the console's rules.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return ErrInvalidName
	}
	// Reserved prefixes keep the console's own resources distinguishable.
	for _, prefix := range []string{"console-", "incus-"} {
		if strings.HasPrefix(name, prefix) {
			return fmt.Errorf("%w: names may not start with %q", ErrInvalidName, prefix)
		}
	}
	return nil
}

// sortInstances orders instances by name so the UI is stable between requests.
func sortInstances(list []Instance) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && strings.ToLower(list[j].Name) < strings.ToLower(list[j-1].Name); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// CountByStatus returns how many instances are running.
func CountByStatus(list []Instance) (running, stopped int) {
	for _, i := range list {
		if i.Running {
			running++
		} else {
			stopped++
		}
	}
	return running, stopped
}

// Totals sums the resources the console has allocated.
type Totals struct {
	Instances   int
	Running     int
	Stopped     int
	VCPUs       int
	MemoryBytes int64
	DiskBytes   int64
	Domains     int
	Snapshots   int
	Backups     int
	BackupBytes int64
}

// Totals aggregates dashboard counters.
func (s *Service) Totals(ctx context.Context, list []Instance) (Totals, error) {
	var t Totals
	t.Instances = len(list)

	for _, i := range list {
		if i.Running {
			t.Running++
		} else {
			t.Stopped++
		}
		t.VCPUs += i.CPU
		t.MemoryBytes += i.MemoryBytes
		t.DiskBytes += i.DiskBytes
	}

	var err error
	if t.Domains, err = s.app.DB.CountDomains(); err != nil {
		return t, err
	}
	if t.Snapshots, err = s.app.DB.CountSnapshots(); err != nil {
		return t, err
	}
	if t.Backups, t.BackupBytes, err = s.app.DB.BackupTotals(); err != nil {
		return t, err
	}

	return t, nil
}

// trackedInstanceNames returns the console-tracked names, used by the monitor.
func (s *Service) trackedInstanceNames() ([]string, error) {
	meta, err := s.app.DB.ListInstanceMeta()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(meta))
	for name := range meta {
		names = append(names, name)
	}
	return names, nil
}

// ensureMetaExists backfills a console record for an instance created outside the
// console so the UI has something to show.
func (s *Service) ensureMetaExists(ctx context.Context, name string) error {
	if _, err := s.app.DB.GetInstanceMeta(name); err == nil {
		return nil
	} else if !errors.Is(err, database.ErrNotFound) {
		return err
	}

	full, err := s.app.Incus.InstanceFull(name)
	if err != nil {
		return err
	}

	inst := full.Instance
	return s.app.DB.SaveInstanceMeta(&models.InstanceMeta{
		Name:        inst.Name,
		Kind:        incus.InstanceKind(inst),
		CPU:         incus.InstanceCPU(inst),
		MemoryMB:    int(incus.InstanceMemoryBytes(inst) >> 20),
		DiskGB:      int(incus.InstanceDiskBytes(inst) >> 30),
		StoragePool: incus.InstanceStoragePool(inst),
	})
}

// lookupImageLabel asks Incus for an image's description. It is one API call,
// so imageLabelCache keeps it from running once per instance per page view.
func (s *Service) lookupImageLabel(fingerprint string) string {
	if fingerprint == "" {
		return "Unknown image"
	}
	img, _, err := s.app.Incus.SDK().GetImage(fingerprint)
	if err != nil || img == nil || img.Properties["description"] == "" {
		return "Unknown image"
	}
	return img.Properties["description"]
}
