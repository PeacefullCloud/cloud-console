// Package backups handles instance backups.
//
// A snapshot is a fast local rollback point. A backup is a portable archive that
// can be exported to independent storage (S3-compatible) and used to re-create
// the instance. The console keeps the two concepts separate on purpose.
package backups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// Job kinds handled by this package.
const (
	JobCreate  = "backup.create"
	JobExport  = "backup.export"
	JobRestore = "backup.restore"
)

// Errors surfaced to the UI.
var (
	ErrNotFound   = errors.New("backup not found")
	ErrNoS3       = errors.New("no S3 target is configured")
	ErrArchiveGon = errors.New("the backup archive is no longer available")
)

// Backup is a backup joined with the state of its archive.
type Backup struct {
	Name         string
	InstanceName string
	Target       string
	Status       string
	Error        string
	SizeBytes    int64
	S3Key        string
	CreatedAt    time.Time
	CreatedBy    string

	LocalPath   string
	LocalExists bool
	InIncus     bool
	InS3        bool

	// Busy is true while a backup job (create, export or restore) is
	// queued or running for this backup. Mutating it meanwhile would
	// corrupt the operation, so the UI withholds those buttons.
	Busy bool
}

// Notifier is told when an instance changes so routing can be refreshed.
type Notifier interface {
	InstanceChanged(ctx context.Context, name string)
}

// Service manages backups and their archives.
type Service struct {
	app      *core.App
	s3       *s3Target
	notifier Notifier
}

// New creates the backup service. When S3 is not configured the service still
// works, storing archives locally only.
func New(app *core.App) *Service {
	s := &Service{app: app}

	if app.Cfg.S3Configured() {
		target, err := newS3Target(app.Cfg)
		if err != nil {
			app.Log.Error("backup storage unavailable", "err", err)
		} else {
			s.s3 = target
		}
	}

	return s
}

// SetNotifier installs the instance change hook.
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// S3Configured reports whether archives can be exported off-host.
func (s *Service) S3Configured() bool { return s.s3 != nil }

// S3Bucket returns the configured bucket name, for display in settings.
func (s *Service) S3Bucket() string { return s.app.Cfg.S3Bucket }

// List returns the backups recorded for an instance.
func (s *Service) List(ctx context.Context, instanceName string) ([]Backup, error) {
	rows, err := s.app.DB.ListBackupsForInstance(instanceName)
	if err != nil {
		return nil, err
	}

	// One lookup serves both the availability flags and the untracked list.
	raw, rawErr := s.app.Incus.Backups(instanceName)
	inIncus := map[string]bool{}
	for _, b := range raw {
		inIncus[b.Name] = true
	}
	lookup := func(string) map[string]bool { return inIncus }

	known := map[string]bool{}
	out := make([]Backup, 0, len(rows))
	for _, r := range rows {
		known[r.Name] = true
		out = append(out, s.enrich(r, lookup))
	}

	// Surface backups that exist in incusd but are not tracked by the console.
	if rawErr == nil {
		for _, b := range raw {
			if known[b.Name] {
				continue
			}
			out = append(out, Backup{
				Name:         b.Name,
				InstanceName: instanceName,
				Target:       "local",
				Status:       "complete",
				CreatedAt:    b.CreatedAt,
				InIncus:      true,
				LocalPath:    s.displayPath(instanceName, b.Name),
			})
		}
	}

	s.checkS3(ctx, out)
	s.markBusy(out)
	return out, nil
}

// InProgress reports whether a backup job (create, export or restore) is
// currently queued or running for the given backup.
func (s *Service) InProgress(instanceName, name string) bool {
	jobs, err := s.app.DB.ListActiveJobs()
	if err != nil {
		return false
	}
	for _, job := range jobs {
		if job.Target != instanceName {
			continue
		}
		switch job.Kind {
		case JobCreate, JobExport, JobRestore:
		default:
			continue
		}
		payload, err := s.app.DB.JobPayload(job.ID)
		if err != nil {
			continue
		}
		var p backupPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			continue
		}
		if p.Name == name {
			return true
		}
	}
	return false
}

// markBusy flags every backup that currently has a job in flight.
func (s *Service) markBusy(out []Backup) {
	jobs, err := s.app.DB.ListActiveJobs()
	if err != nil {
		return
	}
	busy := map[string]bool{}
	for _, job := range jobs {
		switch job.Kind {
		case JobCreate, JobExport, JobRestore:
		default:
			continue
		}
		payload, err := s.app.DB.JobPayload(job.ID)
		if err != nil {
			continue
		}
		var p backupPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			continue
		}
		busy[p.Instance+"\x00"+p.Name] = true
	}
	for i := range out {
		if busy[out[i].InstanceName+"\x00"+out[i].Name] {
			out[i].Busy = true
		}
	}
}

// All returns every backup the console tracks.
func (s *Service) All(ctx context.Context) ([]Backup, error) {
	rows, err := s.app.DB.ListAllBackups()
	if err != nil {
		return nil, err
	}

	// One Incus call per instance, not per backup.
	perInstance := map[string]map[string]bool{}
	lookup := func(instance string) map[string]bool {
		if names, ok := perInstance[instance]; ok {
			return names
		}
		names := map[string]bool{}
		if raw, err := s.app.Incus.Backups(instance); err == nil {
			for _, b := range raw {
				names[b.Name] = true
			}
		}
		perInstance[instance] = names
		return names
	}

	out := make([]Backup, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.enrich(r, lookup))
	}
	s.checkS3(ctx, out)
	s.markBusy(out)
	return out, nil
}

// EnqueueCreate schedules a backup: archive inside Incus, export to disk and
// optionally upload to S3.
func (s *Service) EnqueueCreate(username, instanceName, name string, exportToS3 bool) (*models.Job, error) {
	if _, err := s.app.Incus.Instance(instanceName); err != nil {
		return nil, ErrNotFound
	}
	if exportToS3 && s.s3 == nil {
		return nil, ErrNoS3
	}

	name = strings.TrimSpace(name)
	if name == "" {
		name = SuggestName(instanceName, time.Now())
	}
	if !validBackupName(name) {
		return nil, fmt.Errorf("invalid backup name")
	}

	payload, err := json.Marshal(backupPayload{Instance: instanceName, Name: name, Export: exportToS3})
	if err != nil {
		return nil, err
	}

	return s.app.Jobs.Enqueue(username, JobCreate, instanceName, string(payload))
}

// EnqueueExport schedules an upload of an existing local archive to S3.
func (s *Service) EnqueueExport(username, instanceName, name string) (*models.Job, error) {
	if s.s3 == nil {
		return nil, ErrNoS3
	}
	if _, err := s.app.DB.GetBackup(instanceName, name); err != nil {
		return nil, ErrNotFound
	}

	payload, err := json.Marshal(backupPayload{Instance: instanceName, Name: name, Export: true})
	if err != nil {
		return nil, err
	}
	return s.app.Jobs.Enqueue(username, JobExport, instanceName, string(payload))
}

// EnqueueRestore schedules a restore that re-creates the instance from a backup.
func (s *Service) EnqueueRestore(username, instanceName, name string) (*models.Job, error) {
	if _, err := s.app.DB.GetBackup(instanceName, name); err != nil {
		return nil, ErrNotFound
	}

	payload, err := json.Marshal(backupPayload{Instance: instanceName, Name: name})
	if err != nil {
		return nil, err
	}
	return s.app.Jobs.Enqueue(username, JobRestore, instanceName, string(payload))
}

// Delete removes a backup archive from Incus, local disk and S3, then drops the
// console record.
func (s *Service) Delete(ctx context.Context, instanceName, name string) error {
	if _, err := s.archivePath(instanceName, name); err != nil {
		return err
	}

	var errs []error

	if err := s.app.Incus.DeleteBackup(ctx, instanceName, name); err != nil && !incus.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("incus: %w", err))
	}

	if path, err := s.archivePath(instanceName, name); err != nil {
		errs = append(errs, err)
	} else if err := removeLocal(path); err != nil {
		errs = append(errs, fmt.Errorf("local archive: %w", err))
	}

	if row, err := s.app.DB.GetBackup(instanceName, name); err == nil && row.S3Key != "" && s.s3 != nil {
		if err := s.s3.Delete(ctx, row.S3Key); err != nil {
			errs = append(errs, fmt.Errorf("s3: %w", err))
		}
	}

	if err := s.app.DB.DeleteBackup(instanceName, name); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// --- job handlers ---------------------------------------------------------

type backupPayload struct {
	Instance string `json:"instance"`
	Name     string `json:"name"`
	Export   bool   `json:"export"`
}

func (s *Service) handleCreate(ctx context.Context, job *models.Job) error {
	payload, err := decode(job)
	if err != nil {
		return err
	}

	// Record the attempt so the UI can show a running backup immediately.
	row := &models.Backup{
		InstanceName: payload.Instance,
		Name:         payload.Name,
		Target:       "local",
		Status:       "running",
		CreatedBy:    job.CreatedBy,
	}
	if err := s.app.DB.SaveBackup(row); err != nil {
		return err
	}

	if err := s.createArchive(ctx, job, payload); err != nil {
		_ = s.app.DB.UpdateBackupStatus(payload.Instance, payload.Name, "failed", err.Error(), 0, "")
		return err
	}
	return nil
}

// createArchive performs the archive → export → upload pipeline.
func (s *Service) createArchive(ctx context.Context, job *models.Job, payload backupPayload) error {
	s.app.Jobs.Progress(job.ID, 5, "Creating the backup archive in Incus")

	err := s.app.Incus.CreateBackup(ctx, payload.Instance, incusapi.InstanceBackupsPost{
		Name:                 payload.Name,
		OptimizedStorage:     false, // portable tarball: restorable on any pool
		CompressionAlgorithm: "gzip",
	})
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}

	// Always clean up the incusd-side archive once it is safely on disk.
	defer func() {
		if err := s.app.Incus.DeleteBackup(context.WithoutCancel(ctx), payload.Instance, payload.Name); err != nil {
			s.app.Log.Warn("could not remove incus-side archive", "backup", payload.Name, "err", err)
		}
	}()

	s.app.Jobs.Progress(job.ID, 40, "Exporting the archive")
	path, err := s.archivePath(payload.Instance, payload.Name)
	if err != nil {
		return err
	}

	onProgress := func(current int64) {
		s.app.Jobs.Progress(job.ID, 55+int(current/(64<<20)), "Exporting the archive")
	}

	size, err := s.app.Incus.ExportBackup(ctx, payload.Instance, payload.Name, path, onProgress)
	if err != nil {
		return fmt.Errorf("export backup: %w", err)
	}

	target := "local"
	s3Key := ""

	if payload.Export && s.s3 != nil {
		s.app.Jobs.Progress(job.ID, 80, "Uploading to object storage")

		key := s.s3.Key(payload.Instance, payload.Name)
		uploaded, err := s.s3.Upload(ctx, key, path)
		if err != nil {
			// The local archive is still valid; report the failure but keep it.
			_ = s.app.DB.UpdateBackupStatus(payload.Instance, payload.Name, "failed",
				"upload failed: "+err.Error(), size, "")
			return fmt.Errorf("upload backup: %w", err)
		}
		target = "s3"
		s3Key = key
		if uploaded > 0 {
			size = uploaded
		}
	}

	s.app.Jobs.Progress(job.ID, 95, "Recording the backup")

	return s.app.DB.UpdateBackupStatus(payload.Instance, payload.Name, "complete", "", size, func() string {
		if target == "s3" {
			return s3Key
		}
		return ""
	}())
}

func (s *Service) handleExport(ctx context.Context, job *models.Job) error {
	payload, err := decode(job)
	if err != nil {
		return err
	}
	if s.s3 == nil {
		return ErrNoS3
	}

	path, err := s.archivePath(payload.Instance, payload.Name)
	if err != nil {
		return err
	}

	// Re-create a local archive from Incus when the local copy is missing.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		s.app.Jobs.Progress(job.ID, 10, "Local archive missing, re-creating it")

		row, err := s.app.DB.GetBackup(payload.Instance, payload.Name)
		if err == nil && row.S3Key != "" && !s.s3.Exists(ctx, row.S3Key) {
			return ErrArchiveGon
		}

		if err := s.app.Incus.CreateBackup(ctx, payload.Instance, incusapi.InstanceBackupsPost{
			Name: payload.Name, CompressionAlgorithm: "gzip",
		}); err != nil {
			return fmt.Errorf("re-create backup: %w", err)
		}
		defer func() {
			_ = s.app.Incus.DeleteBackup(context.WithoutCancel(ctx), payload.Instance, payload.Name)
		}()

		if _, err := s.app.Incus.ExportBackup(ctx, payload.Instance, payload.Name, path, nil); err != nil {
			return err
		}
	}

	s.app.Jobs.Progress(job.ID, 50, "Uploading to object storage")

	key := s.s3.Key(payload.Instance, payload.Name)
	size, err := s.s3.Upload(ctx, key, path)
	if err != nil {
		return err
	}

	return s.app.DB.UpdateBackupStatus(payload.Instance, payload.Name, "complete", "", size, key)
}

func (s *Service) handleRestore(ctx context.Context, job *models.Job) error {
	payload, err := decode(job)
	if err != nil {
		return err
	}

	path, err := s.archivePath(payload.Instance, payload.Name)
	if err != nil {
		return err
	}

	// Pull the archive back from object storage when there is no local copy.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		row, err := s.app.DB.GetBackup(payload.Instance, payload.Name)
		if err != nil {
			return ErrNotFound
		}
		if row.S3Key == "" || s.s3 == nil {
			return ErrArchiveGon
		}

		s.app.Jobs.Progress(job.ID, 10, "Downloading the archive from object storage")
		if err := s.s3.Download(ctx, row.S3Key, path); err != nil {
			return err
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	outcome, err := restoreInstance(ctx, s.app.Incus, payload.Instance, f, func(pct int, msg string) {
		s.app.Jobs.Progress(job.ID, pct, msg)
	})
	if err != nil {
		return err
	}
	if outcome.StartErr != nil {
		s.app.Log.Warn("restored instance did not start", "instance", payload.Instance, "err", outcome.StartErr)
	}
	if outcome.Leftover != "" {
		s.app.Log.Warn("replaced instance could not be removed; delete it manually",
			"instance", payload.Instance, "leftover", outcome.Leftover)
	}

	s.app.Jobs.Progress(job.ID, 90, "Waiting for the network")
	if _, err := s.WaitForAddress(ctx, payload.Instance); err != nil {
		s.app.Log.Warn("restored instance has no address yet", "instance", payload.Instance, "err", err)
	}

	if s.notifier != nil {
		s.notifier.InstanceChanged(ctx, payload.Instance)
	}
	return nil
}

// WaitForAddress polls until the instance reports an address.
func (s *Service) WaitForAddress(ctx context.Context, name string) (string, error) {
	deadline := time.Now().Add(90 * time.Second)
	for {
		if full, err := s.app.Incus.InstanceFull(name); err == nil {
			if ip := incus.InstanceIP(full); ip != "" {
				return ip, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for an address")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// RegisterJobHandlers wires this package into the job worker.
func (s *Service) RegisterJobHandlers() {
	s.app.Jobs.Register(JobCreate, s.handleCreate)
	s.app.Jobs.Register(JobExport, s.handleExport)
	s.app.Jobs.Register(JobRestore, s.handleRestore)
}

// --- helpers --------------------------------------------------------------

func decode(job *models.Job) (backupPayload, error) {
	var payload backupPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return payload, fmt.Errorf("decode job payload: %w", err)
	}
	return payload, nil
}

// ErrInvalidName is returned for instance or backup names that could not be a
// safe file name.
var ErrInvalidName = errors.New("invalid instance or backup name")

// safeSegment reports whether name can be one path element without escaping
// its directory: no separators, no leading dot, no control characters.
func safeSegment(name string) bool {
	if name == "" || len(name) > 128 || name[0] == '.' {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

// archivePath is where a backup archive lives on the console's disk. Names
// arrive from URLs and job payloads, so the result is verified to stay inside
// the backups directory before anything reads, writes or deletes it.
func (s *Service) archivePath(instanceName, backupName string) (string, error) {
	if !safeSegment(instanceName) || !safeSegment(backupName) {
		return "", ErrInvalidName
	}

	root := filepath.Join(s.app.Cfg.DataDir, "backups")
	path := filepath.Join(root, instanceName, backupName+".tar.gz")
	if rel, err := filepath.Rel(root, path); err != nil || !filepath.IsLocal(rel) {
		return "", ErrInvalidName
	}
	return path, nil
}

// displayPath is archivePath for listings: names that are not safe simply
// have no local file to show.
func (s *Service) displayPath(instanceName, backupName string) string {
	path, err := s.archivePath(instanceName, backupName)
	if err != nil {
		return ""
	}
	return path
}

// enrich adds archive availability to a database row. incusNames returns the
// backup names Incus holds for an instance and is expected to cache.
func (s *Service) enrich(row models.Backup, incusNames func(instance string) map[string]bool) Backup {
	out := Backup{
		Name:         row.Name,
		InstanceName: row.InstanceName,
		Target:       row.Target,
		Status:       row.Status,
		Error:        row.Error,
		SizeBytes:    row.SizeBytes,
		S3Key:        row.S3Key,
		CreatedAt:    row.CreatedAt,
		CreatedBy:    row.CreatedBy,
		LocalPath:    s.displayPath(row.InstanceName, row.Name),
	}

	if info, err := os.Stat(out.LocalPath); err == nil {
		out.LocalExists = true
		if out.SizeBytes == 0 {
			out.SizeBytes = info.Size()
		}
	}

	out.InIncus = incusNames(row.InstanceName)[row.Name]
	return out
}

// s3CheckConcurrency bounds parallel object lookups.
const s3CheckConcurrency = 8

// checkS3 fills InS3 for every backup that has an offsite copy. Lookups run in
// parallel and are cached briefly, so a long list is not one slow round trip
// per row on every page view.
func (s *Service) checkS3(ctx context.Context, list []Backup) {
	if s.s3 == nil {
		return
	}
	sem := make(chan struct{}, s3CheckConcurrency)
	var wg sync.WaitGroup
	for i := range list {
		if list[i].S3Key == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(b *Backup) {
			defer wg.Done()
			defer func() { <-sem }()
			b.InS3 = s.s3.Exists(ctx, b.S3Key)
		}(&list[i])
	}
	wg.Wait()
}

// SuggestName builds a timestamped backup name.
func SuggestName(instanceName string, t time.Time) string {
	base := strings.Trim(instanceName, "-")
	if len(base) > 32 {
		base = base[:32]
	}
	return base + "-" + t.UTC().Format("20060102-1504")
}

func validBackupName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// archiveDir is the directory holding one instance's local archives.
func (s *Service) archiveDir(instanceName string) (string, error) {
	if !safeSegment(instanceName) {
		return "", ErrInvalidName
	}
	return filepath.Join(s.app.Cfg.DataDir, "backups", instanceName), nil
}

// InstanceRenamed moves local archives to the new instance's directory so the
// renamed instance still finds them. S3 keys are stored per backup and need no
// change.
func (s *Service) InstanceRenamed(oldName, newName string) {
	from, err := s.archiveDir(oldName)
	if err != nil {
		return
	}
	to, err := s.archiveDir(newName)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return
	}
	if err := os.MkdirAll(to, 0o750); err != nil {
		s.app.Log.Warn("could not create backup directory for renamed instance", "dir", to, "err", err)
		return
	}
	for _, e := range entries {
		dst := filepath.Join(to, e.Name())
		if _, err := os.Lstat(dst); err == nil {
			s.app.Log.Warn("not overwriting an existing archive while renaming", "path", dst)
			continue
		}
		if err := os.Rename(filepath.Join(from, e.Name()), dst); err != nil {
			s.app.Log.Warn("could not move backup archive", "archive", e.Name(), "err", err)
		}
	}
	// Fails harmlessly if something was left behind.
	_ = os.Remove(from)
}

// InstanceDeleted removes an instance's local archives and offsite copies. The
// console drops the backup records with the instance, so anything left would be
// unreachable and keep using disk or bucket space.
func (s *Service) InstanceDeleted(ctx context.Context, name string) {
	if s.s3 != nil {
		if rows, err := s.app.DB.ListBackupsForInstance(name); err == nil {
			for _, row := range rows {
				if row.S3Key == "" {
					continue
				}
				if err := s.s3.Delete(ctx, row.S3Key); err != nil {
					s.app.Log.Warn("could not delete offsite backup of a deleted instance", "key", row.S3Key, "err", err)
				}
			}
		}
	}

	dir, err := s.archiveDir(name)
	if err != nil {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		s.app.Log.Warn("could not remove local backups of a deleted instance", "dir", dir, "err", err)
	}
}
