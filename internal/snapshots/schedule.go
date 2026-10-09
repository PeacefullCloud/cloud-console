package snapshots

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/models"
)

// AutoPrefix marks snapshots the scheduler created. Retention only ever
// removes snapshots with this prefix, so manual snapshots are never pruned.
const AutoPrefix = "auto-"

// scheduleHost is the part of the Incus client the scheduler needs.
type scheduleHost interface {
	Instances(ctx context.Context) ([]incusapi.InstanceFull, error)
	Snapshots(name string) ([]incusapi.InstanceSnapshot, error)
	CreateSnapshot(ctx context.Context, instanceName, snapshotName string, stateful bool) error
	DeleteSnapshot(ctx context.Context, instanceName, snapshotName string) error
}

// scheduleStore records and forgets snapshot metadata.
type scheduleStore interface {
	SaveSnapshotMeta(m *models.SnapshotMeta) error
	DeleteSnapshotMeta(instanceName, name string) error
}

// Scheduler takes a snapshot of every instance at a fixed interval and keeps
// the newest N automatic snapshots per instance.
//
// It is stateless: whether an instance is due is decided from the age of its
// newest automatic snapshot, so a restart neither skips nor doubles a run.
type Scheduler struct {
	host     scheduleHost
	store    scheduleStore
	log      *slog.Logger
	interval time.Duration
	keep     int
	now      func() time.Time
}

// NewScheduler builds a scheduler. Interval <= 0 or keep <= 0 means disabled.
func NewScheduler(host scheduleHost, store scheduleStore, log *slog.Logger, interval time.Duration, keep int) *Scheduler {
	return &Scheduler{host: host, store: store, log: log, interval: interval, keep: keep, now: time.Now}
}

// Enabled reports whether the scheduler will do anything.
func (s *Scheduler) Enabled() bool { return s.interval > 0 && s.keep > 0 }

// Run checks for due snapshots until ctx ends. The check itself is cheap
// (one snapshot listing per instance), so it runs more often than the
// interval to keep the schedule accurate.
func (s *Scheduler) Run(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	tick := s.interval / 4
	if tick > 5*time.Minute {
		tick = 5 * time.Minute
	}
	if tick < time.Second {
		tick = time.Second
	}
	// Give the console time to finish starting before the first pass.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.RunOnce(ctx)
			timer.Reset(tick)
		}
	}
}

// RunOnce snapshots every due instance and applies retention.
func (s *Scheduler) RunOnce(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	list, err := s.host.Instances(ctx)
	if err != nil {
		s.log.Warn("scheduled snapshots: could not list instances", "err", err)
		return
	}
	for _, inst := range list {
		if ctx.Err() != nil {
			return
		}
		// A failure on one instance must not stop the others.
		s.handle(ctx, inst.Name)
	}
}

func (s *Scheduler) handle(ctx context.Context, name string) {
	snaps, err := s.host.Snapshots(name)
	if err != nil {
		s.log.Warn("scheduled snapshots: could not list snapshots", "instance", name, "err", err)
		return
	}

	auto := autoSnapshots(snaps)
	if len(auto) == 0 || s.now().Sub(auto[0].CreatedAt) >= s.interval {
		snap := AutoPrefix + s.now().UTC().Format("20060102-150405")
		if err := s.host.CreateSnapshot(ctx, name, snap, false); err != nil {
			s.log.Warn("scheduled snapshot failed", "instance", name, "err", err)
			return
		}
		if err := s.store.SaveSnapshotMeta(&models.SnapshotMeta{
			InstanceName: name, Name: snap, Note: "Scheduled snapshot", CreatedBy: "scheduler",
		}); err != nil {
			s.log.Warn("could not save snapshot metadata", "instance", name, "err", err)
		}
		// Re-read so retention counts the snapshot just taken.
		if snaps, err = s.host.Snapshots(name); err != nil {
			return
		}
		auto = autoSnapshots(snaps)
	}

	for _, old := range auto[min(s.keep, len(auto)):] {
		if err := s.host.DeleteSnapshot(ctx, name, old.Name); err != nil {
			s.log.Warn("scheduled snapshot cleanup failed", "instance", name, "snapshot", old.Name, "err", err)
			continue
		}
		_ = s.store.DeleteSnapshotMeta(name, old.Name)
	}
}

// autoSnapshots returns the scheduler's snapshots, newest first.
func autoSnapshots(all []incusapi.InstanceSnapshot) []incusapi.InstanceSnapshot {
	var out []incusapi.InstanceSnapshot
	for _, snap := range all {
		if strings.HasPrefix(snap.Name, AutoPrefix) {
			out = append(out, snap)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}
