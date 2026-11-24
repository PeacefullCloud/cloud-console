package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

	// SSH access. At least one is required so every instance is reachable
	// as root@<private-ip> without touching the host.
	RootPassword string
	SSHKey       string

	// Optional first-boot launch script (shell script or #cloud-config).
	LaunchScript string

	OwnerID   int64
	OwnerName string
}

const maxLaunchScriptBytes = 64 * 1024

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

	r.RootPassword = strings.TrimSpace(r.RootPassword)
	r.SSHKey = strings.TrimSpace(r.SSHKey)
	if r.RootPassword == "" && r.SSHKey == "" {
		return fmt.Errorf("%w: provide a root password or an SSH public key so the instance is reachable", ErrInvalidSpec)
	}
	if r.RootPassword != "" && len(r.RootPassword) < 8 {
		return fmt.Errorf("%w: the root password must be at least 8 characters", ErrInvalidSpec)
	}
	if r.SSHKey != "" {
		if err := ValidateSSHKey(r.SSHKey); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidSpec, err)
		}
		r.SSHKey = strings.Join(strings.Fields(r.SSHKey), " ")
	}

	r.LaunchScript = strings.ReplaceAll(strings.TrimSpace(r.LaunchScript), "\r\n", "\n")
	if len(r.LaunchScript) > maxLaunchScriptBytes {
		return fmt.Errorf("%w: launch script must be at most %d KB", ErrInvalidSpec, maxLaunchScriptBytes/1024)
	}
	if strings.ContainsRune(r.LaunchScript, 0) {
		return fmt.Errorf("%w: launch script may not contain null bytes", ErrInvalidSpec)
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
	if req.Domain != "" && s.app.Caddy != nil {
		if host := s.app.Caddy.ConsoleHost(); host != "" && req.Domain == host {
			return nil, fmt.Errorf("%w: %s is the console's own address", ErrInvalidSpec, req.Domain)
		}
	}

	// The quota check and the enqueue form one step: without the lock, two
	// simultaneous requests at the limit would both pass.
	s.createMu.Lock()
	defer s.createMu.Unlock()

	if _, err := s.app.Incus.Instance(req.Name); err == nil {
		return nil, ErrNameInUse
	}
	if err := s.checkQuota(context.Background(), req.OwnerID); err != nil {
		return nil, err
	}
	if res, err := s.app.Incus.ServerResources(); err == nil && res != nil {
		if err := checkCapacity(req, int(res.CPU.Total), int64(res.Memory.Total)); err != nil {
			return nil, err
		}
	}

	payload, err := json.Marshal(createPayload{CreateRequest: req})
	if err != nil {
		return nil, err
	}

	return s.app.Jobs.Enqueue(req.OwnerID, username, JobCreate, req.Name, string(payload))
}

// handleCreate runs the create job: build, start, provision SSH, record,
// and route the new instance.
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

	// The payload carries the root password; wipe it now that it is decoded
	// so the secret does not linger in the jobs table.
	defer s.app.Jobs.WipePayload(job.ID)

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
	if req.LaunchScript != "" {
		post.Config["cloud-init.user-data"] = req.LaunchScript
		post.Config["user.user-data"] = req.LaunchScript
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

	// 3b. Enable L2/L3 anti-spoofing (MAC, IPv4, IPv6 filtering) on bridged
	// container NICs when supported by the host bridge so tenants cannot
	// spoof another instance's IP on incusbr0.
	if req.Kind == KindContainer {
		s.applyNICSecurityFilters(ctx, req.Name)
	}

	// 4. Start it.
	s.app.Jobs.Progress(job.ID, 55, "Starting instance")
	if err := s.app.Incus.Start(ctx, req.Name); err != nil {
		return fmt.Errorf("start instance: %w", err)
	}

	// 5. Wait for the guest agent. Containers answer exec immediately;
	// virtual machines need the agent booted before any setup command runs.
	s.app.Jobs.Progress(job.ID, 58, "Waiting for the guest agent")
	if err := s.WaitForAgent(ctx, req.Name, 10*time.Minute); err != nil {
		s.app.Log.Warn("guest agent not ready", "instance", req.Name, "err", err)
	}

	// 6. Configure SSH access in the background while the steps below run:
	// a first-boot package install dominates this step and nothing
	// afterwards depends on SSH being ready, so blocking on it only stalls
	// the progress bar. The result is collected at step 11 before the job
	// finishes, keeping progress messages in order.
	s.app.Jobs.Progress(job.ID, 62, "Setting up SSH access")
	sshDone := make(chan error, 1)
	go func() {
		sshDone <- s.EnsureSSH(ctx, req.Name, image, req.RootPassword, req.SSHKey)
	}()

	// The web terminal's default user is independent of root SSH, so create
	// it alongside SSH rather than adding another serial first-boot wait.
	// sudo is installed by EnsureSSH; the sudoers drop-in is valid either way.
	consoleDone := make(chan error, 1)
	go func() {
		consoleDone <- s.EnsureConsoleUser(ctx, req.Name, req.SSHKey, s.app.Cfg.TerminalLoginUser)
	}()

	var launchDone chan error
	if req.LaunchScript != "" && !strings.HasPrefix(strings.TrimSpace(req.LaunchScript), "#cloud-config") {
		launchDone = make(chan error, 1)
		go func() {
			launchDone <- s.runLaunchScript(ctx, req.Name, req.LaunchScript)
		}()
	}

	// 7. Wait until it has a network address.
	s.app.Jobs.Progress(job.ID, 70, "Waiting for the network to come up")
	ip, err := s.WaitForIP(ctx, req.Name, 90*time.Second)
	if err != nil {
		// The instance exists and runs; a missing address is not fatal.
		s.app.Log.Warn("instance has no address yet", "instance", req.Name, "err", err)
	}

	// 8. Persist console bookkeeping.
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

	// 9. Attach the optional domain.
	consoleHost := ""
	if s.app.Caddy != nil {
		consoleHost = s.app.Caddy.ConsoleHost()
	}
	if req.Domain != "" && req.Domain != consoleHost {
		if _, err := s.app.DB.CreateDomain(req.Name, req.Domain, 80); err != nil {
			s.app.Log.Warn("could not attach domain", "instance", req.Name, "domain", req.Domain, "err", err)
		} else if meta.PrimaryHost == "" {
			meta.PrimaryHost = req.Domain
			_ = s.app.DB.SetPrimaryHost(req.Name, req.Domain)
		}
	}

	// 9b. Give every new instance an instant HTTPS URL on the platform domain
	// when one is configured, so users need no DNS setup to reach it.
	if base := strings.TrimSpace(s.app.Cfg.PlatformDomain); base != "" {
		host := req.Name + "." + strings.TrimPrefix(base, "*.")
		if host != consoleHost {
			if _, err := s.app.DB.CreateDomain(req.Name, host, 80); err != nil {
				s.app.Log.Warn("could not attach platform domain", "instance", req.Name, "domain", host, "err", err)
			} else if meta.PrimaryHost == "" {
				meta.PrimaryHost = host
				_ = s.app.DB.SetPrimaryHost(req.Name, host)
			}
		}
	}

	// 10. Re-render Caddy so the domain reaches the new container.
	s.app.Jobs.Progress(job.ID, 95, "Applying web routing")
	s.notifyChanged(ctx, req.Name)

	// 11. Collect the SSH setup result. A failure here is not fatal: the
	// instance runs and stays manageable with incus exec, but the user is
	// told. Name the wait honestly: routing is done, SSH is what is left.
	s.app.Jobs.Progress(job.ID, 95, "Waiting for SSH access to finish")
	sshErr, consoleErr := <-sshDone, <-consoleDone
	if sshErr != nil {
		s.app.Log.Warn("ssh setup incomplete", "instance", req.Name, "err", sshErr)
		s.app.Jobs.Progress(job.ID, 96, "SSH setup incomplete: "+sshErr.Error())
	} else {
		s.app.Jobs.Progress(job.ID, 96, "SSH access ready for root")
	}
	if consoleErr != nil {
		s.app.Log.Warn("console user setup incomplete", "instance", req.Name, "err", consoleErr)
		s.app.Jobs.Progress(job.ID, 96, "Web console user setup incomplete: "+consoleErr.Error())
	}
	if launchDone != nil {
		if launchErr := <-launchDone; launchErr != nil {
			s.app.Log.Warn("launch script failed", "instance", req.Name, "err", launchErr)
			s.app.Jobs.Progress(job.ID, 98, "Launch script failed: "+launchErr.Error())
		} else {
			s.app.Jobs.Progress(job.ID, 98, "Launch script finished")
		}
	}

	if ip != "" {
		s.app.Jobs.Progress(job.ID, 100, "Instance is running at "+ip)
	}
	return nil
}

// runLaunchScript writes and executes a user-supplied shell launch script
// inside the instance, logging output to /var/log/cloud-console-launch.log.
func (s *Service) runLaunchScript(ctx context.Context, name, script string) error {
	script = strings.ReplaceAll(strings.TrimSpace(script), "\r\n", "\n")
	if script == "" || strings.HasPrefix(script, "#cloud-config") {
		return nil
	}
	if !strings.HasPrefix(script, "#!") {
		script = "#!/bin/sh\nset -e\n" + script
	}
	script += "\n"
	const scriptPath = "/var/lib/cloud-console-launch.sh"
	if err := s.app.Incus.PushFile(ctx, name, scriptPath, script, 0, 0, os.FileMode(0o700)); err != nil {
		return err
	}
	_, err := s.app.Incus.Exec(ctx, name, "", "/bin/sh", "-c",
		scriptPath+" >/var/log/cloud-console-launch.log 2>&1")
	return err
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

	return s.app.Jobs.Enqueue(s.ownerID(name), username, JobRebuild, name, string(payload))
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

	if inst, err := s.app.Incus.Instance(payload.Name); err == nil && inst != nil && !strings.EqualFold(inst.Status, "Running") {
		s.app.Jobs.Progress(job.ID, 40, "Starting rebuilt instance")
		_ = s.app.Incus.Start(ctx, payload.Name)
	}

	// Update recorded OS image metadata in SQLite so the UI shows the new OS.
	ownerID := s.ownerID(payload.Name)
	if meta, err := s.app.DB.GetInstanceMeta(payload.Name); err == nil {
		meta.Image = image.Alias
		meta.ImageLabel = image.Label
		_ = s.app.DB.SaveInstanceMeta(meta)
	}

	// Look up the owner's saved SSH public keys so rebuild restores SSH access.
	var sshKey string
	if ownerID > 0 {
		if keys, err := s.app.DB.ListSSHKeysForUser(ownerID); err == nil && len(keys) > 0 {
			sshKey = keys[0].PublicKey
		}
	}

	s.app.Jobs.Progress(job.ID, 55, "Waiting for the guest agent")
	if err := s.WaitForAgent(ctx, payload.Name, 5*time.Minute); err != nil {
		s.app.Log.Warn("rebuild: guest agent not ready", "instance", payload.Name, "err", err)
	}

	// Re-provision SSH and the web terminal user concurrently alongside WaitForIP.
	s.app.Jobs.Progress(job.ID, 65, "Re-provisioning SSH and console user")
	sshDone := make(chan error, 1)
	go func() {
		if sshKey != "" {
			sshDone <- s.EnsureSSH(ctx, payload.Name, image, "", sshKey)
			return
		}
		if err := s.ensureSSHServer(ctx, payload.Name, image); err != nil {
			sshDone <- err
			return
		}
		sshDone <- s.enableSSH(ctx, payload.Name, image, false)
	}()

	consoleDone := make(chan error, 1)
	go func() {
		consoleDone <- s.EnsureConsoleUser(ctx, payload.Name, sshKey, s.app.Cfg.TerminalLoginUser)
	}()

	s.app.Jobs.Progress(job.ID, 80, "Waiting for the instance to come back")
	if _, err := s.WaitForIP(ctx, payload.Name, 90*time.Second); err != nil {
		s.app.Log.Warn("rebuild: instance has no address yet", "instance", payload.Name, "err", err)
	}
	s.syncDedicatedIPs(ctx, payload.Name)

	if err := <-sshDone; err != nil {
		s.app.Log.Warn("rebuild: ssh provisioning incomplete", "instance", payload.Name, "err", err)
	}
	if err := <-consoleDone; err != nil {
		s.app.Log.Warn("rebuild: console user provisioning incomplete", "instance", payload.Name, "err", err)
	}

	s.notifyChanged(ctx, payload.Name)
	return nil
}

// applyNICSecurityFilters enables MAC, IPv4, and IPv6 spoofing protection on
// bridged container NICs when supported by the host bridge. If the host kernel
// lacks bridge netfilter support or uses an unmanaged bridge, it logs and
// continues without failing instance creation.
func (s *Service) applyNICSecurityFilters(ctx context.Context, name string) {
	inst, err := s.app.Incus.Instance(name)
	if err != nil || inst == nil {
		return
	}
	var nicName string
	var nicCfg map[string]string
	for devName, dev := range inst.ExpandedDevices {
		if dev["type"] == "nic" && (dev["network"] != "" || dev["nictype"] == "bridged") {
			nicName = devName
			nicCfg = dev
			break
		}
	}
	if nicName == "" {
		return
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
	nic := map[string]string{}
	for k, v := range nicCfg {
		nic[k] = v
	}
	nic["security.mac_filtering"] = "true"
	nic["security.ipv4_filtering"] = "true"
	nic["security.ipv6_filtering"] = "true"
	put.Devices[nicName] = nic
	if err := s.app.Incus.UpdateInstance(ctx, name, put); err != nil {
		s.app.Log.Debug("could not enable NIC anti-spoofing filters", "instance", name, "err", err)
	}
}

// --- simple lifecycle operations -----------------------------------------

// Start boots an instance.
func (s *Service) Start(ctx context.Context, name string) error {
	return s.app.Incus.Start(ctx, name)
}

// Stop shuts an instance down cleanly.
func (s *Service) Stop(ctx context.Context, name string) error {
	return s.app.Incus.Stop(ctx, name)
}

// ForceStop powers an instance off immediately.
func (s *Service) ForceStop(ctx context.Context, name string) error {
	return s.app.Incus.ForceStop(ctx, name)
}

// Restart reboots an instance cleanly.
func (s *Service) Restart(ctx context.Context, name string) error {
	return s.app.Incus.Restart(ctx, name)
}

// Delete removes an instance, its console metadata and its monitoring history.
func (s *Service) Delete(ctx context.Context, name string) error {
	if s.hasActiveJob(name) {
		return ErrBusy
	}
	if err := s.app.Incus.DeleteInstance(ctx, name); err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}

	// Before the rows go: the keeper needs them to find offsite copies.
	if s.archives != nil {
		s.archives.InstanceDeleted(ctx, name)
	}
	s.releaseDedicatedIPs(ctx, name)
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
	if s.hasActiveJob(oldName) {
		return ErrBusy
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
	if err := s.app.DB.RenameResticSnapshots(oldName, newName); err != nil {
		s.app.Log.Warn("could not move restic snapshot metadata", "err", err)
	}
	if err := s.app.DB.RenameMetrics(oldName, newName); err != nil {
		s.app.Log.Warn("could not move metrics history", "err", err)
	}
	s.renameDedicatedIPs(ctx, oldName, newName)
	if s.archives != nil {
		s.archives.InstanceRenamed(oldName, newName)
	}

	s.notifyChanged(ctx, newName)
	return nil
}

// Limits is a partial update of an instance's resources. A nil field leaves
// that resource as it is; a pointer to 0 removes the CPU or memory limit.
// Disk is grow-only, so 0 is not meaningful there and is treated as unchanged.
type Limits struct {
	CPU      *int
	MemoryMB *int
	DiskGB   *int
}

// UpdateLimits applies new CPU, memory and disk limits.
func (s *Service) UpdateLimits(ctx context.Context, name string, lim Limits) error {
	if err := lim.validate(); err != nil {
		return err
	}
	if s.app != nil && s.app.Incus != nil {
		if res, err := s.app.Incus.ServerResources(); err == nil && res != nil {
			var cpuReq, memReq int
			if lim.CPU != nil {
				cpuReq = *lim.CPU
			}
			if lim.MemoryMB != nil {
				memReq = *lim.MemoryMB
			}
			if err := checkCapacity(CreateRequest{CPU: cpuReq, MemoryMB: memReq}, int(res.CPU.Total), int64(res.Memory.Total)); err != nil {
				return err
			}
		}
	}

	inst, err := s.app.Incus.Instance(name)
	if err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}

	put, err := limitsPut(inst, lim)
	if err != nil {
		return err
	}
	if err := s.app.Incus.UpdateInstance(ctx, name, put); err != nil {
		return err
	}

	// Refresh only the resource columns so image/owner metadata survives.
	meta := &models.InstanceMeta{
		Name:        name,
		Kind:        incus.InstanceKind(*inst),
		CPU:         limitCPU(put),
		MemoryMB:    int(incus.ParseBytes(put.Config["limits.memory"]) >> 20),
		DiskGB:      int(incus.ParseBytes(put.Devices["root"]["size"]) >> 30),
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

func (l Limits) validate() error {
	if l.CPU != nil && (*l.CPU < 0 || *l.CPU > MaxCPU) {
		return fmt.Errorf("%w: CPU must be between 0 and %d", ErrInvalidSpec, MaxCPU)
	}
	if l.MemoryMB != nil && (*l.MemoryMB < 0 || (*l.MemoryMB > 0 && *l.MemoryMB < MinMemoryMB) || *l.MemoryMB > MaxMemoryMB) {
		return fmt.Errorf("%w: memory must be 0 or between %d MB and %d MB", ErrInvalidSpec, MinMemoryMB, MaxMemoryMB)
	}
	if l.DiskGB != nil && (*l.DiskGB < 0 || *l.DiskGB > MaxDiskGB) {
		return fmt.Errorf("%w: disk must be between 0 and %d GB", ErrInvalidSpec, MaxDiskGB)
	}
	return nil
}

func limitCPU(put incusapi.InstancePut) int {
	n, _ := strconv.Atoi(put.Config["limits.cpu"])
	return n
}

// limitsPut builds the update for an instance. It starts from the instance's
// own config and devices; a root disk that comes from a profile is copied into
// the instance (with its pool and path) before its size is changed, because a
// bare disk device with no pool is not a usable root.
func limitsPut(inst *incusapi.Instance, lim Limits) (incusapi.InstancePut, error) {
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

	if lim.CPU != nil {
		if *lim.CPU > 0 {
			put.Config["limits.cpu"] = strconv.Itoa(*lim.CPU)
		} else {
			delete(put.Config, "limits.cpu")
		}
	}
	if lim.MemoryMB != nil {
		if *lim.MemoryMB > 0 {
			put.Config["limits.memory"] = fmt.Sprintf("%dMiB", *lim.MemoryMB)
		} else {
			delete(put.Config, "limits.memory")
		}
	}

	if lim.DiskGB != nil && *lim.DiskGB > 0 {
		root := map[string]string{}
		source, ok := put.Devices["root"]
		if !ok {
			source, ok = inst.ExpandedDevices["root"]
		}
		if !ok {
			return put, fmt.Errorf("%w: the instance has no root disk to resize", ErrInvalidSpec)
		}
		for k, v := range source {
			root[k] = v
		}
		current := incus.ParseBytes(root["size"])
		// The form shows whole GiB (rounded down), so the displayed value of a
		// 10.5 GiB disk is 10: resubmitting it is a no-op, not a shrink.
		if int64(*lim.DiskGB) == current>>30 {
			return put, nil
		}
		if int64(*lim.DiskGB)<<30 < current {
			return put, fmt.Errorf("%w: the root disk cannot be shrunk", ErrInvalidSpec)
		}
		root["size"] = fmt.Sprintf("%dGiB", *lim.DiskGB)
		put.Devices["root"] = root
	}
	return put, nil
}

// Allowed mount prefixes for attached data volumes so a tenant cannot mount
// a volume over /etc, /bin, /usr, /var, /root, /proc, /sys, or /dev.
var allowedVolumeMountPrefixes = []string{"/mnt/", "/srv/", "/data/", "/opt/"}

func validVolumeMountPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.Contains(raw, "..") {
		return "", fmt.Errorf("%w: mount path must be an absolute path under /mnt, /srv, /data, or /opt", ErrInvalidSpec)
	}
	cleaned := filepath.Clean(raw)
	for _, p := range allowedVolumeMountPrefixes {
		if strings.HasPrefix(cleaned, p) && len(cleaned) > len(p) {
			return cleaned, nil
		}
	}
	return "", fmt.Errorf("%w: mount path must be under /mnt/, /srv/, /data/, or /opt/ (for example /mnt/data)", ErrInvalidSpec)
}

// AttachVolume creates a custom storage volume in the instance's storage pool
// and attaches it at mountPath.
func (s *Service) AttachVolume(ctx context.Context, instanceName, volName, mountPath string, sizeGB int) error {
	if s.hasActiveJob(instanceName) {
		return ErrBusy
	}
	volName = strings.ToLower(strings.TrimSpace(volName))
	if err := ValidateName(volName); err != nil {
		return fmt.Errorf("%w: volume name: %v", ErrInvalidSpec, err)
	}
	if sizeGB < 1 || sizeGB > MaxDiskGB {
		return fmt.Errorf("%w: volume size must be between 1 GB and %d GB", ErrInvalidSpec, MaxDiskGB)
	}
	cleanMount, err := validVolumeMountPath(mountPath)
	if err != nil {
		return err
	}

	inst, err := s.app.Incus.Instance(instanceName)
	if err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}

	devName := "disk-" + volName
	if _, exists := inst.Devices[devName]; exists {
		return fmt.Errorf("%w: a volume named %q is already attached", ErrInvalidSpec, volName)
	}
	for _, dev := range inst.ExpandedDevices {
		if dev["type"] == "disk" && filepath.Clean(dev["path"]) == cleanMount {
			return fmt.Errorf("%w: another disk is already mounted at %s", ErrInvalidSpec, cleanMount)
		}
	}

	pool := ""
	if root, ok := inst.ExpandedDevices["root"]; ok {
		pool = root["pool"]
	}
	if pool == "" {
		pool = s.defaultStoragePool()
	}
	if pool == "" {
		return fmt.Errorf("%w: no storage pool is configured", ErrInvalidSpec)
	}

	fullVolName := instanceName + "-" + volName
	createdVol := false
	if err := s.app.Incus.CreateVolume(pool, fullVolName, sizeGB); err != nil {
		if !incus.IsAlreadyExists(err) {
			return fmt.Errorf("create volume: %w", err)
		}
	} else {
		createdVol = true
	}

	put := cloneInstancePut(inst)
	put.Devices[devName] = map[string]string{
		"type":   "disk",
		"pool":   pool,
		"source": fullVolName,
		"path":   cleanMount,
	}
	if err := s.app.Incus.UpdateInstance(ctx, instanceName, put); err != nil {
		if createdVol {
			_ = s.app.Incus.DeleteVolume(pool, fullVolName)
		}
		return err
	}
	return nil
}

// DetachVolume removes an attached custom disk device from an instance and
// optionally deletes the backing custom volume from the storage pool.
func (s *Service) DetachVolume(ctx context.Context, instanceName, devName string, deleteVol bool) error {
	if s.hasActiveJob(instanceName) {
		return ErrBusy
	}
	devName = strings.TrimSpace(devName)
	if devName == "" || devName == "root" {
		return fmt.Errorf("%w: the root disk cannot be detached", ErrInvalidSpec)
	}

	inst, err := s.app.Incus.Instance(instanceName)
	if err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	dev, ok := inst.Devices[devName]
	if !ok || dev["type"] != "disk" {
		return fmt.Errorf("%w: disk %q is not attached to this instance", ErrInvalidSpec, devName)
	}

	put := cloneInstancePut(inst)
	delete(put.Devices, devName)
	if err := s.app.Incus.UpdateInstance(ctx, instanceName, put); err != nil {
		return err
	}

	if deleteVol && dev["pool"] != "" && strings.HasPrefix(dev["source"], instanceName+"-") {
		_ = s.app.Incus.DeleteVolume(dev["pool"], dev["source"])
	}
	return nil
}

// Port range allowed for per-instance TCP/UDP proxy port forwards.
const (
	MinForwardListenPort = 10000
	MaxForwardListenPort = 60000
)

// AddPortForward exposes a guest TCP/UDP port on a host port via an Incus
// proxy device.
func (s *Service) AddPortForward(ctx context.Context, instanceName, proto string, listenPort, targetPort int) error {
	if s.hasActiveJob(instanceName) {
		return ErrBusy
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto != "tcp" && proto != "udp" {
		return fmt.Errorf("%w: protocol must be tcp or udp", ErrInvalidSpec)
	}
	if listenPort < MinForwardListenPort || listenPort > MaxForwardListenPort {
		return fmt.Errorf("%w: external port must be between %d and %d", ErrInvalidSpec, MinForwardListenPort, MaxForwardListenPort)
	}
	if targetPort < 1 || targetPort > 65535 {
		return fmt.Errorf("%w: instance port must be between 1 and 65535", ErrInvalidSpec)
	}

	inst, err := s.app.Incus.Instance(instanceName)
	if err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}

	listenSpec := fmt.Sprintf("%s:0.0.0.0:%d", proto, listenPort)
	if all, lerr := s.app.Incus.Instances(ctx); lerr == nil {
		for _, other := range all {
			for _, dev := range other.ExpandedDevices {
				if dev["type"] == "proxy" && strings.EqualFold(dev["listen"], listenSpec) {
					return fmt.Errorf("%w: %s port %d is already forwarded", ErrInvalidSpec, strings.ToUpper(proto), listenPort)
				}
			}
		}
	}

	devName := fmt.Sprintf("port-%s-%d", proto, listenPort)
	put := cloneInstancePut(inst)
	put.Devices[devName] = map[string]string{
		"type":    "proxy",
		"listen":  listenSpec,
		"connect": fmt.Sprintf("%s:127.0.0.1:%d", proto, targetPort),
	}
	return s.app.Incus.UpdateInstance(ctx, instanceName, put)
}

// RemovePortForward removes a TCP/UDP proxy port forward from an instance.
func (s *Service) RemovePortForward(ctx context.Context, instanceName, devName string) error {
	if s.hasActiveJob(instanceName) {
		return ErrBusy
	}
	inst, err := s.app.Incus.Instance(instanceName)
	if err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	dev, ok := inst.Devices[devName]
	if !ok || dev["type"] != "proxy" {
		return fmt.Errorf("%w: port forward %q was not found", ErrInvalidSpec, devName)
	}
	put := cloneInstancePut(inst)
	delete(put.Devices, devName)
	return s.app.Incus.UpdateInstance(ctx, instanceName, put)
}

func cloneInstancePut(inst *incusapi.Instance) incusapi.InstancePut {
	put := incusapi.InstancePut{
		Architecture: inst.Architecture,
		Description:  inst.Description,
		Profiles:     append([]string(nil), inst.Profiles...),
		Config:       incusapi.ConfigMap{},
		Devices:      incusapi.DevicesMap{},
	}
	for k, v := range inst.Config {
		put.Config[k] = v
	}
	for k, v := range inst.Devices {
		devCopy := make(map[string]string, len(v))
		for dk, dv := range v {
			devCopy[dk] = dv
		}
		put.Devices[k] = devCopy
	}
	return put
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

// hasActiveJob reports whether a queued or running job targets the instance.
// Deleting or renaming it underneath such a job would make the job fail
// halfway or write to the wrong place.
func (s *Service) hasActiveJob(name string) bool {
	jobs, err := s.app.DB.ListActiveJobs()
	if err != nil {
		return false
	}
	for _, j := range jobs {
		if j.Target == name {
			return true
		}
	}
	return false
}

// checkQuota enforces the host-wide instance limit and the owner's personal
// quota. Instances still being created count, or a burst of requests would slip
// past either limit.
func (s *Service) checkQuota(ctx context.Context, ownerID int64) error {
	if err := s.checkGlobalQuota(ctx); err != nil {
		return err
	}
	return s.checkOwnerQuota(ownerID)
}

// checkGlobalQuota refuses a new instance once CONSOLE_MAX_INSTANCES is
// reached. A limit of 0 means unlimited.
func (s *Service) checkGlobalQuota(ctx context.Context) error {
	limit := s.app.Cfg.MaxInstances
	if limit <= 0 {
		return nil
	}

	existing, err := s.app.Incus.Instances(ctx)
	if err != nil {
		return fmt.Errorf("could not count existing instances: %w", err)
	}
	names := make(map[string]bool, len(existing))
	for i := range existing {
		names[existing[i].Name] = true
	}

	// A queued or running create job may not have an Incus instance yet.
	if jobs, err := s.app.DB.ListActiveJobs(); err == nil {
		for _, j := range jobs {
			if j.Kind == JobCreate {
				names[j.Target] = true
			}
		}
	}

	if len(names) >= limit {
		return fmt.Errorf("%w: this console is limited to %d instances", ErrQuota, limit)
	}
	return nil
}

// checkOwnerQuota refuses a new instance once the owner's per-user limit is
// reached. A limit of 0 means unlimited.
func (s *Service) checkOwnerQuota(ownerID int64) error {
	if ownerID <= 0 {
		return nil
	}

	user, err := s.app.DB.GetUser(ownerID)
	if err != nil {
		return fmt.Errorf("could not read the instance owner: %w", err)
	}
	if user.MaxInstances <= 0 {
		return nil
	}

	used, err := s.app.DB.CountInstanceSlotsByOwner(ownerID, JobCreate)
	if err != nil {
		return fmt.Errorf("could not count the owner's instances: %w", err)
	}
	if used >= user.MaxInstances {
		return fmt.Errorf("%w: this account is limited to %d instances", ErrQuota, user.MaxInstances)
	}
	return nil
}

// checkCapacity refuses a single instance that could never run on this host.
// It is deliberately not a sum over all instances: CPU and memory are
// routinely overcommitted, so only an impossible request is an error.
func checkCapacity(req CreateRequest, hostCPUs int, hostMemory int64) error {
	if hostCPUs > 0 && req.CPU > hostCPUs {
		return fmt.Errorf("%w: %d vCPUs requested but the host has %d", ErrLimitsTooBig, req.CPU, hostCPUs)
	}
	if hostMemory > 0 && int64(req.MemoryMB)<<20 > hostMemory {
		return fmt.Errorf("%w: %d MB of memory requested but the host has %d MB",
			ErrLimitsTooBig, req.MemoryMB, hostMemory>>20)
	}
	return nil
}
