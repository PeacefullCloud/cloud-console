package snapshots

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/models"
)

type fakeHost struct {
	snaps   map[string][]incusapi.InstanceSnapshot
	created []string
	deleted []string
	failOn  string
	now     time.Time
}

func (f *fakeHost) Instances(context.Context) ([]incusapi.InstanceFull, error) {
	var out []incusapi.InstanceFull
	for name := range f.snaps {
		out = append(out, incusapi.InstanceFull{Instance: incusapi.Instance{Name: name}})
	}
	return out, nil
}

func (f *fakeHost) Snapshots(name string) ([]incusapi.InstanceSnapshot, error) {
	return append([]incusapi.InstanceSnapshot(nil), f.snaps[name]...), nil
}

func (f *fakeHost) CreateSnapshot(_ context.Context, inst, snap string, _ bool) error {
	if inst == f.failOn {
		return errors.New("boom")
	}
	f.created = append(f.created, inst+"/"+snap)
	f.snaps[inst] = append(f.snaps[inst], incusapi.InstanceSnapshot{Name: snap, CreatedAt: f.now})
	return nil
}

func (f *fakeHost) DeleteSnapshot(_ context.Context, inst, snap string) error {
	f.deleted = append(f.deleted, inst+"/"+snap)
	kept := f.snaps[inst][:0]
	for _, s := range f.snaps[inst] {
		if s.Name != snap {
			kept = append(kept, s)
		}
	}
	f.snaps[inst] = kept
	return nil
}

type fakeStore struct{ saved, removed []string }

func (f *fakeStore) SaveSnapshotMeta(m *models.SnapshotMeta) error {
	f.saved = append(f.saved, m.InstanceName+"/"+m.Name)
	return nil
}

func (f *fakeStore) DeleteSnapshotMeta(i, n string) error {
	f.removed = append(f.removed, i+"/"+n)
	return nil
}

func newSched(h *fakeHost, st *fakeStore, interval time.Duration, keep int) *Scheduler {
	s := NewScheduler(h, st, slog.New(slog.NewTextHandler(io.Discard, nil)), interval, keep)
	s.now = func() time.Time { return h.now }
	return s
}

func snap(name string, at time.Time) incusapi.InstanceSnapshot {
	return incusapi.InstanceSnapshot{Name: name, CreatedAt: at}
}

func TestSchedulerSnapshotsDueInstancesOnly(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	h := &fakeHost{now: now, snaps: map[string][]incusapi.InstanceSnapshot{
		"new":    nil,
		"fresh":  {snap("auto-a", now.Add(-time.Hour))},
		"stale":  {snap("auto-b", now.Add(-25*time.Hour))},
		"manual": {snap("before-upgrade", now.Add(-72*time.Hour))},
	}}
	st := &fakeStore{}
	newSched(h, st, 24*time.Hour, 7).RunOnce(context.Background())

	got := map[string]bool{}
	for _, c := range h.created {
		got[c[:len(c)-len("/auto-20260110-120000")]] = true
	}
	for _, want := range []string{"new", "stale", "manual"} {
		if !got[want] {
			t.Errorf("%s was not snapshotted (created: %v)", want, h.created)
		}
	}
	if got["fresh"] {
		t.Error("a recently snapshotted instance was snapshotted again")
	}
	if len(st.saved) != 3 {
		t.Errorf("metadata saved %d times, want 3", len(st.saved))
	}
}

func TestSchedulerRetentionOnlyPrunesAutoSnapshots(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	h := &fakeHost{now: now, snaps: map[string][]incusapi.InstanceSnapshot{
		"web": {
			snap("auto-1", now.Add(-5*time.Hour)),
			snap("auto-2", now.Add(-4*time.Hour)),
			snap("auto-3", now.Add(-3*time.Hour)),
			snap("auto-4", now.Add(-2*time.Hour)),
			snap("keep-me", now.Add(-100*time.Hour)),
		},
	}}
	st := &fakeStore{}
	// Interval is longer than the newest snapshot's age, so only pruning runs.
	newSched(h, st, 24*time.Hour, 2).RunOnce(context.Background())

	if len(h.created) != 0 {
		t.Fatalf("unexpected snapshot: %v", h.created)
	}
	want := map[string]bool{"web/auto-2": true, "web/auto-1": true}
	if len(h.deleted) != 2 || !want[h.deleted[0]] || !want[h.deleted[1]] {
		t.Errorf("deleted %v, want the two oldest auto snapshots", h.deleted)
	}
	for _, s := range h.snaps["web"] {
		if s.Name == "keep-me" {
			return
		}
	}
	t.Error("manual snapshot was pruned")
}

func TestSchedulerNewSnapshotCountsTowardsRetention(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	h := &fakeHost{now: now, snaps: map[string][]incusapi.InstanceSnapshot{
		"web": {snap("auto-old", now.Add(-48*time.Hour))},
	}}
	newSched(h, &fakeStore{}, 24*time.Hour, 1).RunOnce(context.Background())

	if len(h.snaps["web"]) != 1 || h.snaps["web"][0].Name == "auto-old" {
		t.Errorf("want only the new snapshot left, got %+v", h.snaps["web"])
	}
}

func TestSchedulerFailureDoesNotStopOthers(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	h := &fakeHost{now: now, failOn: "bad", snaps: map[string][]incusapi.InstanceSnapshot{"bad": nil, "good": nil}}
	st := &fakeStore{}
	newSched(h, st, time.Hour, 3).RunOnce(context.Background())

	if len(h.created) != 1 || len(st.saved) != 1 {
		t.Errorf("created=%v saved=%v", h.created, st.saved)
	}
}

func TestSchedulerDisabled(t *testing.T) {
	h := &fakeHost{snaps: map[string][]incusapi.InstanceSnapshot{"web": nil}}
	for _, s := range []*Scheduler{newSched(h, &fakeStore{}, 0, 3), newSched(h, &fakeStore{}, time.Hour, 0)} {
		if s.Enabled() {
			t.Error("should be disabled")
		}
		s.RunOnce(context.Background())
	}
	if len(h.created) != 0 {
		t.Error("a disabled scheduler took a snapshot")
	}
}
