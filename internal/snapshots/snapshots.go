// Package snapshots manages Incus snapshots.
//
// A snapshot is a fast, local rollback point. Backups (which can be exported to
// independent storage) live in the backups package; the distinction matters and
// is reflected in the UI.
package snapshots

import (
	"context"
	"errors"
	"strings"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// Errors surfaced to the UI.
var (
	ErrNotFound  = errors.New("snapshot not found")
	ErrInvalid   = errors.New("invalid snapshot name")
	ErrNameInUse = errors.New("a snapshot with that name already exists")
)

// Snapshot is a snapshot joined with its console metadata.
type Snapshot struct {
	Name         string
	InstanceName string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	Stateful     bool
	Note         string
	CreatedBy    string
	Tracked      bool
}

// Service manages snapshots.
type Service struct {
	app *core.App
}

// New creates the snapshot service.
func New(app *core.App) *Service {
	return &Service{app: app}
}

// List returns an instance's snapshots, newest first.
func (s *Service) List(instanceName string) ([]Snapshot, error) {
	raw, err := s.app.Incus.Snapshots(instanceName)
	if err != nil {
		if incus.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	meta, err := s.app.DB.ListSnapshotMetaForInstance(instanceName)
	if err != nil {
		return nil, err
	}

	out := make([]Snapshot, 0, len(raw))
	for _, snap := range raw {
		out = append(out, merge(snap, instanceName, meta))
	}

	// Newest first keeps "restore the last good one" a single click.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// All returns every snapshot the console knows about.
func (s *Service) All() ([]Snapshot, error) {
	meta, err := s.app.DB.ListAllSnapshotMeta()
	if err != nil {
		return nil, err
	}

	byInstance := map[string][]models.SnapshotMeta{}
	for _, m := range meta {
		byInstance[m.InstanceName] = append(byInstance[m.InstanceName], m)
	}

	var out []Snapshot
	for name, list := range byInstance {
		raw, err := s.app.Incus.Snapshots(name)
		if err != nil {
			// Instance may have been removed outside the console.
			continue
		}

		index := map[string]models.SnapshotMeta{}
		for _, m := range list {
			index[m.Name] = m
		}
		for _, snap := range raw {
			out = append(out, merge(snap, name, index))
		}
	}

	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Create takes a snapshot and records who asked for it.
func (s *Service) Create(ctx context.Context, instanceName, name, note, username string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		name = SuggestName(time.Now())
	}
	if !validSnapshotName(name) {
		return ErrInvalid
	}

	if _, err := s.app.Incus.Instance(instanceName); err != nil {
		return ErrNotFound
	}

	if err := s.app.Incus.CreateSnapshot(ctx, instanceName, name, false); err != nil {
		if incus.IsAlreadyExists(err) {
			return ErrNameInUse
		}
		return err
	}

	if err := s.app.DB.SaveSnapshotMeta(&models.SnapshotMeta{
		InstanceName: instanceName,
		Name:         name,
		Note:         note,
		CreatedBy:    username,
	}); err != nil {
		s.app.Log.Warn("could not save snapshot metadata", "err", err)
	}

	return nil
}

// Delete removes a snapshot.
func (s *Service) Delete(ctx context.Context, instanceName, name string) error {
	if err := s.app.Incus.DeleteSnapshot(ctx, instanceName, name); err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	return s.app.DB.DeleteSnapshotMeta(instanceName, name)
}

// Restore rolls an instance back to a snapshot.
func (s *Service) Restore(ctx context.Context, instanceName, name string) error {
	if err := s.app.Incus.RestoreSnapshot(ctx, instanceName, name); err != nil {
		if incus.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Rename changes a snapshot's name.
func (s *Service) Rename(ctx context.Context, instanceName, oldName, newName string) error {
	newName = strings.TrimSpace(newName)
	if !validSnapshotName(newName) {
		return ErrInvalid
	}
	if err := s.app.Incus.RenameSnapshot(ctx, instanceName, oldName, newName); err != nil {
		return err
	}

	// Carry console metadata across to the new name.
	if meta, err := s.app.DB.GetSnapshotMeta(instanceName, oldName); err == nil {
		meta.Name = newName
		_ = s.app.DB.SaveSnapshotMeta(meta)
	}
	return s.app.DB.DeleteSnapshotMeta(instanceName, oldName)
}

// SuggestName builds a timestamped snapshot name such as snap-20261007-1530.
func SuggestName(t time.Time) string {
	return "snap-" + t.UTC().Format("20060102-1504")
}

func merge(snap incusapi.InstanceSnapshot, instanceName string, meta map[string]models.SnapshotMeta) Snapshot {
	out := Snapshot{
		Name:         snap.Name,
		InstanceName: instanceName,
		CreatedAt:    snap.CreatedAt,
		ExpiresAt:    snap.ExpiresAt,
		Stateful:     snap.Stateful,
	}

	if m, ok := meta[snap.Name]; ok {
		out.Tracked = true
		out.Note = m.Note
		out.CreatedBy = m.CreatedBy
	}
	return out
}

func validSnapshotName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
