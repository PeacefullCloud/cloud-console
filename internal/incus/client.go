// Package incus wraps the official Incus Go SDK.
//
// Everything the console needs to talk to Incus goes through this package so the
// rest of the application never touches raw HTTP or shell commands. Incus is
// the source of truth for infrastructure state (running state, IPs, resource
// usage, snapshots, storage).
package incus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/ioprogress"
)

// DefaultSocketPath is where incusd listens on a standard install.
const DefaultSocketPath = "/var/lib/incus/unix.socket"

// Client is a thin, context-aware wrapper around the Incus SDK.
type Client struct {
	srv     incusclient.InstanceServer
	socket  string
	project string
	log     *slog.Logger
}

// Connect dials the local incusd over its Unix socket.
//
// The console talks to Incus over the local socket only. The Incus API is never
// exposed to the internet.
func Connect(socket, project string, log *slog.Logger) (*Client, error) {
	if socket == "" {
		socket = DefaultSocketPath
	}

	srv, err := incusclient.ConnectIncusUnix(socket, &incusclient.ConnectionArgs{
		UserAgent: "peaceful-cloud-console",
	})
	if err != nil {
		return nil, fmt.Errorf("connect to incus socket %s: %w", socket, err)
	}

	if project != "" {
		srv = srv.UseProject(project)
	}

	c := &Client{srv: srv, socket: socket, project: project, log: log}

	// Fail fast with a useful message when the daemon is unreachable.
	if _, _, err := srv.GetServer(); err != nil {
		return nil, fmt.Errorf("talk to incus at %s: %w", socket, err)
	}

	return c, nil
}

// Socket returns the Unix socket path in use.
func (c *Client) Socket() string { return c.socket }

// Project returns the active Incus project ("" means default).
func (c *Client) Project() string { return c.project }

// SDK exposes the underlying SDK server for advanced use.
func (c *Client) SDK() incusclient.InstanceServer { return c.srv }

// --- Server ---------------------------------------------------------------

// Server returns the Incus server record.
func (c *Client) Server() (*api.Server, error) {
	server, _, err := c.srv.GetServer()
	return server, err
}

// ServerResources returns host CPU/memory/storage capacity.
func (c *Client) ServerResources() (*api.Resources, error) {
	return c.srv.GetServerResources()
}

// StoragePools lists configured storage pools.
func (c *Client) StoragePools() ([]api.StoragePool, error) {
	return c.srv.GetStoragePools()
}

// Networks lists configured networks (bridges, physical links, ...).
func (c *Client) Networks() ([]api.Network, error) {
	return c.srv.GetNetworks()
}

// StoragePoolResources returns used/total space for a pool.
func (c *Client) StoragePoolResources(name string) (*api.ResourcesStoragePool, error) {
	return c.srv.GetStoragePoolResources(name)
}

// ImageAliases lists locally available image aliases.
func (c *Client) ImageAliases() ([]api.ImageAliasesEntry, error) {
	return c.srv.GetImageAliases()
}

// --- Instances ------------------------------------------------------------

// Instances returns every instance together with its live state.
func (c *Client) Instances(ctx context.Context) ([]api.InstanceFull, error) {
	return c.srv.GetInstancesFull(api.InstanceTypeAny)
}

// Instance returns a single instance.
func (c *Client) Instance(name string) (*api.Instance, error) {
	inst, _, err := c.srv.GetInstance(name)
	return inst, err
}

// InstanceFull returns a single instance with its live state.
func (c *Client) InstanceFull(name string) (*api.InstanceFull, error) {
	inst, _, err := c.srv.GetInstanceFull(name)
	return inst, err
}

// InstanceState returns live CPU/memory/disk/network usage for an instance.
func (c *Client) InstanceState(name string) (*api.InstanceState, error) {
	state, _, err := c.srv.GetInstanceState(name)
	return state, err
}

// CreateInstance creates an instance and waits for the operation to finish.
// When req.Start is true the instance is started as part of creation.
func (c *Client) CreateInstance(ctx context.Context, req api.InstancesPost) error {
	op, err := c.srv.CreateInstance(req)
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "create instance "+req.Name)
}

// UpdateInstance applies configuration changes (resource limits, restore, ...).
func (c *Client) UpdateInstance(ctx context.Context, name string, put api.InstancePut) error {
	inst, etag, err := c.srv.GetInstance(name)
	if err != nil {
		return err
	}

	// Preserve current values for fields the caller did not set.
	merged := put
	if merged.Architecture == "" {
		merged.Architecture = inst.Architecture
	}
	if merged.Description == "" {
		merged.Description = inst.Description
	}
	// A restore request must not clobber the profile list.
	if merged.Restore != "" && merged.Profiles == nil {
		merged.Profiles = inst.Profiles
		merged.Devices = nil
		merged.Config = nil
	}

	op, err := c.srv.UpdateInstance(name, merged, etag)
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "update instance "+name)
}

// SetState changes the running state: start, stop, restart, freeze, unfreeze.
func (c *Client) SetState(ctx context.Context, name, action string, force bool, timeout time.Duration) error {
	op, err := c.srv.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  action,
		Force:   force,
		Timeout: int(timeout.Seconds()),
	}, "")
	if err != nil {
		return err
	}
	return c.wait(ctx, op, action+" instance "+name)
}

// Start boots an instance.
func (c *Client) Start(ctx context.Context, name string) error {
	return c.SetState(ctx, name, "start", false, 5*time.Minute)
}

// Stop shuts an instance down, forcing it after the timeout expires.
func (c *Client) Stop(ctx context.Context, name string) error {
	return c.SetState(ctx, name, "stop", true, 2*time.Minute)
}

// Restart restarts an instance.
func (c *Client) Restart(ctx context.Context, name string) error {
	return c.SetState(ctx, name, "restart", true, 5*time.Minute)
}

// DeleteInstance removes an instance and all of its snapshots.
func (c *Client) DeleteInstance(ctx context.Context, name string) error {
	op, err := c.srv.DeleteInstance(name)
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "delete instance "+name)
}

// RenameInstance changes an instance's name.
func (c *Client) RenameInstance(ctx context.Context, oldName, newName string) error {
	op, err := c.srv.RenameInstance(oldName, api.InstancePost{Name: newName})
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "rename instance "+oldName)
}

// RebuildInstance replaces an instance's root filesystem from an image while
// keeping its name, configuration and (optionally) its snapshots.
func (c *Client) RebuildInstance(ctx context.Context, name string, src api.InstanceSource) error {
	op, err := c.srv.RebuildInstance(name, api.InstanceRebuildPost{Source: src})
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "rebuild instance "+name)
}

// --- Snapshots ------------------------------------------------------------

// Snapshots lists an instance's snapshots.
func (c *Client) Snapshots(name string) ([]api.InstanceSnapshot, error) {
	return c.srv.GetInstanceSnapshots(name)
}

// CreateSnapshot takes a snapshot of an instance.
func (c *Client) CreateSnapshot(ctx context.Context, instanceName, snapshotName string, stateful bool) error {
	op, err := c.srv.CreateInstanceSnapshot(instanceName, api.InstanceSnapshotsPost{
		Name:     snapshotName,
		Stateful: stateful,
	})
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "snapshot "+instanceName+"/"+snapshotName)
}

// DeleteSnapshot removes a snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, instanceName, snapshotName string) error {
	op, err := c.srv.DeleteInstanceSnapshot(instanceName, snapshotName)
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "delete snapshot "+instanceName+"/"+snapshotName)
}

// RenameSnapshot renames a snapshot.
func (c *Client) RenameSnapshot(ctx context.Context, instanceName, oldName, newName string) error {
	op, err := c.srv.RenameInstanceSnapshot(instanceName, oldName, api.InstanceSnapshotPost{Name: newName})
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "rename snapshot "+instanceName+"/"+oldName)
}

// RestoreSnapshot rolls an instance back to a snapshot.
func (c *Client) RestoreSnapshot(ctx context.Context, instanceName, snapshotName string) error {
	return c.UpdateInstance(ctx, instanceName, api.InstancePut{Restore: snapshotName})
}

// --- Backups --------------------------------------------------------------

// Backups lists the incusd-side backups for an instance.
func (c *Client) Backups(name string) ([]api.InstanceBackup, error) {
	return c.srv.GetInstanceBackups(name)
}

// CreateBackup creates a backup archive inside incusd.
func (c *Client) CreateBackup(ctx context.Context, instanceName string, backup api.InstanceBackupsPost) error {
	op, err := c.srv.CreateInstanceBackup(instanceName, backup)
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "backup "+instanceName+"/"+backup.Name)
}

// DeleteBackup removes an incusd-side backup archive.
func (c *Client) DeleteBackup(ctx context.Context, instanceName, backupName string) error {
	op, err := c.srv.DeleteInstanceBackup(instanceName, backupName)
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "delete backup "+instanceName+"/"+backupName)
}

// ExportBackup streams an incusd-side backup to a local file and returns its size.
//
// The backup is written to disk first so it can be uploaded to independent
// storage or re-imported later, exactly like the console's "backup vs snapshot"
// split requires.
func (c *Client) ExportBackup(instanceName, backupName, destPath string, onProgress func(int64)) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return 0, err
	}

	f, err := os.Create(destPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	req := &incusclient.BackupFileRequest{BackupFile: f}
	if onProgress != nil {
		// The SDK reports progress through a callback; forward the bytes sent.
		req.ProgressHandler = func(p ioprogress.ProgressData) { onProgress(p.TransferredBytes) }
	}

	resp, err := c.srv.GetInstanceBackupFile(instanceName, backupName, req)
	if err != nil {
		return 0, err
	}

	if err := f.Sync(); err != nil {
		return 0, err
	}

	if resp != nil && resp.Size > 0 {
		return resp.Size, nil
	}
	info, err := os.Stat(destPath)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// ImportBackup re-creates an instance from a backup archive.
func (c *Client) ImportBackup(ctx context.Context, name string, r io.Reader, pool string) error {
	args := incusclient.InstanceBackupArgs{BackupFile: r, Name: name, PoolName: pool}

	// The SDK needs a seekable reader for retries, so buffer to a temp file
	// when the caller cannot provide one.
	op, err := c.srv.CreateInstanceFromBackup(args)
	if err != nil {
		return err
	}
	return c.wait(ctx, op, "import backup "+name)
}

// --- Console / files ------------------------------------------------------

// ConsoleLog returns the instance's console log.
func (c *Client) ConsoleLog(name string) (io.ReadCloser, error) {
	return c.srv.GetInstanceConsoleLog(name, nil)
}

// Logfiles lists the instance's log files.
func (c *Client) Logfiles(name string) ([]string, error) {
	return c.srv.GetInstanceLogfiles(name)
}

// Logfile reads one instance log file.
func (c *Client) Logfile(name, filename string) (io.ReadCloser, error) {
	return c.srv.GetInstanceLogfile(name, filename)
}

// Events subscribes to server events.
func (c *Client) Events() (*incusclient.EventListener, error) {
	return c.srv.GetEvents()
}

// --- helpers --------------------------------------------------------------

// wait blocks on an Incus operation until it completes or the context ends.
func (c *Client) wait(ctx context.Context, op incusclient.Operation, what string) error {
	if op == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- op.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	case <-ctx.Done():
		_ = op.Cancel()
		return fmt.Errorf("%s: %w", what, ctx.Err())
	}
}

// IsNotFound reports whether an Incus error means "no such object".
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

// IsAlreadyExists reports whether an Incus error means "already exists".
func IsAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "already in use")
}

// ErrUnavailable is returned when Incus cannot be reached.
var ErrUnavailable = errors.New("incus is unavailable")
