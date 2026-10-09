package jobs

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
)

func newManager(t *testing.T, workers int) (*Manager, *database.DB) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := database.Open(filepath.Join(t.TempDir(), "c.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, log, workers), db
}

func start(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
}

func waitStatus(t *testing.T, db *database.DB, id, want string) *models.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, err := db.GetJob(id); err == nil && j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := db.GetJob(id)
	t.Fatalf("job %s never reached %q (now %+v)", id, want, j)
	return nil
}

func TestJobRunsToCompletion(t *testing.T) {
	m, db := newManager(t, 2)
	m.Register("ok", func(ctx context.Context, j *models.Job) error { return nil })
	start(t, m)

	job, err := m.Enqueue("alice", "ok", "web", "{}")
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, db, job.ID, "done")
}

func TestPanickingJobFailsWithoutKillingTheManager(t *testing.T) {
	m, db := newManager(t, 1)
	m.Register("boom", func(ctx context.Context, j *models.Job) error { panic("kaboom") })
	m.Register("ok", func(ctx context.Context, j *models.Job) error { return nil })
	start(t, m)

	bad, _ := m.Enqueue("a", "boom", "web", "{}")
	failed := waitStatus(t, db, bad.ID, "failed")
	if failed.Error == "" {
		t.Error("the failure carries no explanation")
	}

	good, _ := m.Enqueue("a", "ok", "web", "{}")
	waitStatus(t, db, good.ID, "done")
}

func TestJobsForOneTargetNeverOverlap(t *testing.T) {
	m, db := newManager(t, 4)
	var active, peak atomic.Int32
	m.Register("work", func(ctx context.Context, j *models.Job) error {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		active.Add(-1)
		return nil
	})
	start(t, m)

	var ids []string
	for i := 0; i < 4; i++ {
		j, err := m.Enqueue("a", "work", "same-instance", "{}")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	for _, id := range ids {
		waitStatus(t, db, id, "done")
	}
	if peak.Load() != 1 {
		t.Errorf("%d jobs ran at once on one instance", peak.Load())
	}
}

func TestJobsForDifferentTargetsRunTogether(t *testing.T) {
	m, db := newManager(t, 2)
	gate := make(chan struct{})
	var running atomic.Int32
	m.Register("work", func(ctx context.Context, j *models.Job) error {
		if running.Add(1) == 2 {
			close(gate)
		}
		select {
		case <-gate:
		case <-time.After(3 * time.Second):
			return context.DeadlineExceeded
		}
		return nil
	})
	start(t, m)

	a, _ := m.Enqueue("a", "work", "one", "{}")
	b, _ := m.Enqueue("a", "work", "two", "{}")
	waitStatus(t, db, a.ID, "done")
	waitStatus(t, db, b.ID, "done")
}

func TestQueuedJobSurvivesRestart(t *testing.T) {
	m, db := newManager(t, 1)
	m.Register("ok", func(ctx context.Context, j *models.Job) error {
		if j.Payload != `{"x":1}` {
			t.Errorf("payload lost across restart: %q", j.Payload)
		}
		return nil
	})

	// Written by a "previous run" that stopped before it got to this job.
	if err := db.CreateJob(&models.Job{ID: "left-over", Kind: "ok", Target: "web", Payload: `{"x":1}`, Status: "queued", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	start(t, m)
	waitStatus(t, db, "left-over", "done")
}

func TestRunningJobIsFailedAtRestart(t *testing.T) {
	m, db := newManager(t, 1)
	m.Register("ok", func(ctx context.Context, j *models.Job) error { return nil })
	if err := db.CreateJob(&models.Job{ID: "cut-off", Kind: "ok", Target: "web", Status: "queued", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.StartJob("cut-off"); err != nil {
		t.Fatal(err)
	}
	start(t, m)
	waitStatus(t, db, "cut-off", "failed")
}

func TestEnqueueAfterStopDoesNotPanic(t *testing.T) {
	m, _ := newManager(t, 1)
	m.Register("ok", func(ctx context.Context, j *models.Job) error { return nil })
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Stop()
	if _, err := m.Enqueue("a", "ok", "web", "{}"); err == nil {
		t.Error("enqueue after Stop succeeded")
	}
	m.Stop() // idempotent
}

func TestEnqueueUnknownKind(t *testing.T) {
	m, _ := newManager(t, 1)
	if _, err := m.Enqueue("a", "nope", "web", "{}"); err == nil {
		t.Error("unknown kind accepted")
	}
}
