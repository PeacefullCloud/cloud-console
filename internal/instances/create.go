package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// Job kinds handled by this package.
const (
	JobCreate  = "instance.create"
	JobRebuild = "instance.rebuild"
)

// Resource bounds the wizard enforces.
const (
	MinCPU      = 1
	MaxCPU      = 64
	MinMemoryMB = 128
	MaxMemoryMB = 1024 * 1024 // 1 TiB
	MinDiskGB   = 2
	MaxDiskGB   = 10 * 1024 // 10 TiB

	// MinVMDiskGB is the smallest root disk for a virtual machine. VM
	// images are gigabytes before first boot, so a container-sized disk
	// would fill immediately.
	MinVMDiskGB = 10
)

// Instance kinds the wizard offers.
const (
	KindContainer = "container"
	KindVM        = "virtual-machine"
)

// CreateRequest is the validated input from the "Create instance" wizard.
type CreateRequest struct {
	Name        string
	ImageID     string
	Kind        string // container | virtual-machine
	CPU         int
	MemoryMB    int
	DiskGB      int
	StoragePool string
	Domain      string
	Notes       string

	OwnerID   int64
	OwnerName string
}

// createPayload is the JSON stored with an instance.create job.
type createPayload struct {
	CreateRequest
}

// Validate checks a create request before anything is created.
func (r *CreateRequest) Validate() error {
	r.Name = strings.ToLower(strings.TrimSpace(r.Name))
	if err := ValidateName(r.Name); err != nil {
		return err
	}

	if r.CPU < MinCPU || r.CPU > MaxCPU {
		return fmt.Errorf("%w: CPU must be between %d and %d", ErrInvalidSpec, MinCPU, MaxCPU)
	}
	if r.MemoryMB < MinMemoryMB || r.MemoryMB > MaxMemoryMB {
		return fmt.Errorf("%w: memory must be between %d MB and %d MB", ErrInvalidSpec, MinMemoryMB, MaxMemoryMB)
	}
	if r.DiskGB < MinDiskGB || r.DiskGB > MaxDiskGB {
		return fmt.Errorf("%w: disk must be between %d GB and %d GB", ErrInvalidSpec, MinDiskGB, MaxDiskGB)
	}

	image, ok := incus.LookupImage(r.ImageID)
	if !ok {
		return fmt.Errorf("%w: unknown image %q", ErrInvalidSpec, r.ImageID)
	}

	if r.Kind == "" {
		r.Kind = KindContainer
	}
	if r.Kind != KindContainer && r.Kind != KindVM {
		return fmt.Errorf("%w: unknown instance type %q", ErrInvalidSpec, r.Kind)
	}
	if r.Kind == KindVM {
		if !image.SupportsVM {
			return fmt.Errorf("%w: %q is available as a container only", ErrInvalidSpec, image.Label)
		}
		if r.DiskGB < MinVMDiskGB {
			return fmt.Errorf("%w: virtual machines need at least %d GB of disk", ErrInvalidSpec, MinVMDiskGB)
		}
	}

	r.Domain = strings.ToLower(strings.TrimSpace(r.Domain))
	if r.Domain != "" && !validHostname(r.Domain) {
		return fmt.Errorf("%w: %q is not a valid domain", ErrInvalidSpec, r.Domain)
	}

	return nil
}

// EnqueueCreate validates the request and schedules instance creation.
//
// Creating an instance pulls an image and boots a container, so it runs in the
// background and the UI follows the job.
func (s *Service) EnqueueCreate(username string, req CreateRequest) (*models.Job, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	if _, err := s.app.Incus.Instance(req.Name); err == nil {
		return nil, ErrNameInUse
	}

	payload, err := json.Marshal(createPayload{CreateRequest: req})
	if err != nil {
		return nil, err
	}

	return s.app.Jobs.Enqueue(username, JobCreate, req.Name, string(payload))
}

// handleCreate runs the create job: the nine steps from the V1 plan.
func (s *Service) handleCreate(ctx context.Context, job *models.Job) error {
	var payload createPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("decode job payload: %w", err)
	}
	req := payload.CreateRequest

	image, ok := incus.LookupImage(req.ImageID)
	if !ok {
		return fmt.Errorf("%w: unknown image %q", ErrInvalidSpec, req.ImageID)
	}

	// 1. Build the instance definition.
	s.app.Jobs.Progress(job.ID, 5, "Preparing instance definition")

	pool := req.StoragePool
	if pool == "" {
		pool = s.defaultStoragePool()
	}

	post := incusapi.InstancesPost{
		Name:   req.Name,
		Type:   incusapi.InstanceType(req.Kind),
		Source: image.Source(),
		InstancePut: incusapi.InstancePut{
			Profiles: []string{"default"},
			Config:   map[string]string{},
			Devices:  incusapi.DevicesMap{},
		},
	}

	// 2. Apply resource limits.
	if req.CPU > 0 {
		post.Config["limits.cpu"] = fmt.Sprintf("%d", req.CPU)
	}
	if req.MemoryMB > 0 {
		post.Config["limits.memory"] = fmt.Sprintf("%dMiB", req.MemoryMB)
	}
	if image.Category == "app" {
		// Application images run a full stack inside the container.
		post.Config["security.nesting"] = "true"
	}

	root := map[string]string{"type": "disk", "path": "/"}
	if pool != "" {
		root["pool"] = pool
	}
	if req.DiskGB > 0 {
		root["size"] = fmt.Sprintf("%dGiB", req.DiskGB)
	}
	post.Devices["root"] = root

	// 3. Create the instance in Incus.
	s.app.Jobs.Progress(job.ID, 15, "Creating Incus instance from "+image.Label)
	if err := s.app.Incus.CreateInstance(ctx, post); err != nil {
		if incus.IsAlreadyExists(err) {
			return ErrNameInUse
		}
		return fmt.Errorf("create instance: %w", err)
	}

	// 4. Start it.
	s.app.Jobs.Progress(job.ID, 55, "Starting instance")
	if err := s.app.Incus.Start(ctx, req.Name); err != nil {
		return fmt.Errorf("start instance: %w", err)
	}

	// 5. Wait until it has a network address.
	s.app.Jobs.Progress(job.ID, 70, "Waiting for the network to come up")
	ip, err := s.WaitForIP(ctx, req.Name, 90*time.Second)
	if err != nil {
		// The instance exists and runs; a missing address is not fatal.
		s.app.Log.Warn("instance has no address yet", "instance", req.Name, "err", err)
	}

	// 6. Persist console bookkeeping.
	s.app.Jobs.Progress(job.ID, 85, "Recording instance in the console")
	meta := &models.InstanceMeta{
		Name:        req.Name,
		Image:       image.Alias,
		ImageLabel:  image.Label,
		Kind:        req.Kind,
		CPU:         req.CPU,
		MemoryMB:    req.MemoryMB,
		DiskGB:      req.DiskGB,
		StoragePool: pool,
		OwnerID:     req.OwnerID,
		Notes:       req.Notes,
	}
	if err := s.app.DB.SaveInstanceMeta(meta); err != nil {
		return fmt.Errorf("save instance metadata: %w", err)
	}

	// 7. Attach the optional domain.
	if req.Domain != "" {
		if _, err := s.app.DB.CreateDomain(req.Name, req.Domain, 80); err != nil {
			s.app.Log.Warn("could not attach domain", "instance", req.Name, "domain", req.Domain, "err", err)
		}
	}

	// 8. Re-render Caddy so the domain reaches the new container.
	s.app.Jobs.Progress(job.ID, 95, "Applying web routing")
	s.notifyChanged(ctx, req.Name)

	if ip != "" {
		s.app.Jobs.Progress(job.ID, 100, "Instance is running at "+ip)
	}
	return nil
}

// WaitForIP polls Incus until the instance reports a routable address.
func (s *Service) WaitForIP(ctx context.Context, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if full, err := s.app.Incus.InstanceFull(name); err == nil {
			if ip := incus.InstanceIP(full); ip != "" {
				return ip, nil
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for %s to get an address", name)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// EnqueueRebuild schedules a rebuild of an instance from an image.
func (s *Service) EnqueueRebuild(username, name, imageID string) (*models.Job, error) {
	if _, ok := incus.LookupImage(imageID); !ok {
		return nil, fmt.Errorf("%w: unknown image %q", ErrInvalidSpec, imageID)
	}
	if _, err := s.app.Incus.Instance(name); err != nil {
		return nil, ErrNotFound
	}

	payload, err := json.Marshal(map[string]string{"name": name, "image": imageID})
	if err != nil {
		return nil, err
	}

	return s.app.Jobs.Enqueue(username, JobRebuild, name, string(payload))
}

func (s *Service) handleRebuild(ctx context.Context, job *models.Job) error {
	var payload struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("decode job payload: %w", err)
	}

	image, ok := incus.LookupImage(payload.Image)
	if !ok {
		return fmt.Errorf("%w: unknown image %q", ErrInvalidSpec, payload.Image)
	}

	s.app.Jobs.Progress(job.ID, 10, "Rebuilding from "+image.Label)
	if err := s.app.Incus.RebuildInstance(ctx, payload.Name, image.Source()); err != nil {
		return fmt.Errorf("rebuild instance: %w", err)
	}

	s.app.Jobs.Progress(job.ID, 90, "Waiting for the instance to come back")
	if _, err := s.WaitForIP(ctx, payload.Name, 90*time.Second); err != nil {
		s.app.Log.Warn("rebuild: instance has no address yet", "instance", payload.Name, "err", err)
	}

	s.notifyChanged(ctx, payload.Name)
	return nil
}

// --- simple lifecycle operations -----------------------------------------

// Start boots an instance.
func (s *Service) Start(ctx context.Context, name string) error {
	return s.app.Incus.Start(ctx, name)
}

// Stop powers an instance off.
func (s *Service) Stop(ctx context.Context, name string) error {
	return s.app.Incus.Stop(ctx, name)
}

// Restart reboots an instance.
func (s *Service) Restart(ctx context.Context, name string) error {
	return s.app.Incus.Restart(ctx, name)
}

// Delete removes an instance, its console metadata and its monitoring history.
func (s *Service) Delete(ctx context.Context, name string) error {
	if err := s.app.Incus.DeleteInstance(ctx, name); err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}

	s.app.DB.DeleteDomainsForInstance(name)
	s.app.DB.DeleteSnapshotsForInstance(name)
	s.app.DB.DeleteBackupsForInstance(name)
	s.app.DB.DeleteMetricsForInstance(name)
	s.app.DB.DeleteInstanceMeta(name)

	s.notifyChanged(ctx, name)
	return nil
}

// Rename changes an instance's name and keeps console metadata consistent.
func (s *Service) Rename(ctx context.Context, oldName, newName string) error {
	newName = strings.ToLower(strings.TrimSpace(newName))
	if err := ValidateName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}

	if err := s.app.Incus.RenameInstance(ctx, oldName, newName); err != nil {
		if incus.IsAlreadyExists(err) {
			return ErrNameInUse
		}
		return err
	}

	// Move console metadata across to the new name.
	if meta, err := s.app.DB.GetInstanceMeta(oldName); err == nil {
		meta.Name = newName
		_ = s.app.DB.SaveInstanceMeta(meta)
		_ = s.app.DB.DeleteInstanceMeta(oldName)
	}
	if domains, err := s.app.DB.ListDomainsForInstance(oldName); err == nil {
		for _, d := range domains {
			_ = s.app.DB.DeleteDomain(d.ID)
			_, _ = s.app.DB.CreateDomain(newName, d.Domain, d.Port)
		}
	}
	if err := s.app.DB.RenameSnapshots(oldName, newName); err != nil {
		s.app.Log.Warn("could not move snapshot metadata", "err", err)
	}
	if err := s.app.DB.RenameBackups(oldName, newName); err != nil {
		s.app.Log.Warn("could not move backup metadata", "err", err)
	}

	s.notifyChanged(ctx, newName)
	return nil
}

// UpdateLimits applies new CPU, memory and disk limits.
func (s *Service) UpdateLimits(ctx context.Context, name string, cpu, memoryMB, diskGB int) error {
	if cpu < 0 || cpu > MaxCPU {
		return fmt.Errorf("%w: CPU must be between 0 and %d", ErrInvalidSpec, MaxCPU)
	}
	if memoryMB < 0 || memoryMB > MaxMemoryMB {
		return fmt.Errorf("%w: memory must be between 0 and %d MB", ErrInvalidSpec, MaxMemoryMB)
	}
	if diskGB < 0 || diskGB > MaxDiskGB {
		return fmt.Errorf("%w: disk must be between 0 and %d GB", ErrInvalidSpec, MaxDiskGB)
	}

	inst, err := s.app.Incus.Instance(name)
	if err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}

	put := incusapi.InstancePut{
		Architecture: inst.Architecture,
		Description:  inst.Description,
		Profiles:     inst.Profiles,
		Config:       incusapi.ConfigMap{},
		Devices:      incusapi.DevicesMap{},
	}
	for k, v := range inst.Config {
		put.Config[k] = v
	}
	for k, v := range inst.Devices {
		put.Devices[k] = v
	}

	if cpu > 0 {
		put.Config["limits.cpu"] = fmt.Sprintf("%d", cpu)
	} else {
		delete(put.Config, "limits.cpu")
	}

	if memoryMB > 0 {
		put.Config["limits.memory"] = fmt.Sprintf("%dMiB", memoryMB)
	} else {
		delete(put.Config, "limits.memory")
	}

	// Disk growth only; shrinking a root device is not possible.
	if diskGB > 0 {
		root, ok := put.Devices["root"]
		if !ok {
			root = map[string]string{"type": "disk", "path": "/"}
		}
		current := incus.ParseBytes(root["size"])
		if int64(diskGB)<<30 < current {
			return fmt.Errorf("%w: the root disk cannot be shrunk", ErrInvalidSpec)
		}
		root["size"] = fmt.Sprintf("%dGiB", diskGB)
		put.Devices["root"] = root
	}

	if err := s.app.Incus.UpdateInstance(ctx, name, put); err != nil {
		return err
	}

	// Refresh only the resource columns so image/owner metadata survives.
	meta := &models.InstanceMeta{
		Name:        name,
		Kind:        incus.InstanceKind(*inst),
		CPU:         cpu,
		MemoryMB:    memoryMB,
		DiskGB:      diskGB,
		StoragePool: incus.InstanceStoragePool(*inst),
		OwnerID:     s.ownerID(name),
	}
	if existing, err := s.app.DB.GetInstanceMeta(name); err == nil {
		meta.Image = existing.Image
		meta.ImageLabel = existing.ImageLabel
		meta.PrimaryHost = existing.PrimaryHost
		meta.Notes = existing.Notes
	}
	if err := s.app.DB.SaveInstanceMeta(meta); err != nil {
		s.app.Log.Warn("could not save instance limits", "err", err)
	}

	return nil
}

// defaultStoragePool picks the pool to use when the caller does not choose one.
func (s *Service) defaultStoragePool() string {
	if pool := s.app.DB.GetSetting("storage.pool", ""); pool != "" {
		return pool
	}
	pools, err := s.app.Incus.StoragePools()
	if err != nil || len(pools) == 0 {
		return ""
	}
	// Prefer a pool named "default", otherwise take the first.
	for _, p := range pools {
		if p.Name == "default" {
			return p.Name
		}
	}
	return pools[0].Name
}

// DefaultStoragePool is the exported form used by the wizard.
func (s *Service) DefaultStoragePool() string { return s.defaultStoragePool() }

// StoragePools lists the pools available to the wizard.
func (s *Service) StoragePools() ([]incusapi.StoragePool, error) {
	return s.app.Incus.StoragePools()
}

func (s *Service) ownerID(name string) int64 {
	meta, err := s.app.DB.GetInstanceMeta(name)
	if err != nil {
		return 0
	}
	return meta.OwnerID
}

// notifyChanged re-renders web routing after an instance change.
func (s *Service) notifyChanged(ctx context.Context, name string) {
	if s.notifier != nil {
		s.notifier.InstanceChanged(ctx, name)
	}
}

// RegisterJobHandlers wires this package into the job worker.
func (s *Service) RegisterJobHandlers() {
	s.app.Jobs.Register(JobCreate, s.handleCreate)
	s.app.Jobs.Register(JobRebuild, s.handleRebuild)
}

func validHostname(host string) bool {
	if len(host) < 4 || len(host) > 253 {
		return false
	}
	if !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
