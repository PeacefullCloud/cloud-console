package database

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/peaceful/cloud-console/internal/models"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "console.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newJob(t *testing.T, db *DB, id string) {
	t.Helper()
	err := db.CreateJob(&models.Job{ID: id, Kind: "instance.create", Target: "web", Payload: `{"root_password":"hunter2"}`, Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
}

func payloadOf(t *testing.T, db *DB, id string) string {
	t.Helper()
	p, err := db.JobPayload(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestJobPayloadSurvivesWhileActive(t *testing.T) {
	db := openTestDB(t)
	newJob(t, db, "a")
	if err := db.StartJob("a"); err != nil {
		t.Fatal(err)
	}
	if payloadOf(t, db, "a") == "" {
		t.Fatal("a running job still needs its payload")
	}
}

func TestFinishJobWipesPayload(t *testing.T) {
	for _, status := range []string{"done", "failed"} {
		db := openTestDB(t)
		newJob(t, db, "a")
		if err := db.FinishJob("a", status, "x", ""); err != nil {
			t.Fatal(err)
		}
		if got := payloadOf(t, db, "a"); got != "" {
			t.Errorf("%s job kept its payload: %q", status, got)
		}
	}
}

func TestFailStaleJobsOnlyFailsRunningOnes(t *testing.T) {
	db := openTestDB(t)
	newJob(t, db, "queued")
	newJob(t, db, "running")
	if err := db.StartJob("running"); err != nil {
		t.Fatal(err)
	}

	n, err := db.FailStaleJobs()
	if err != nil || n != 1 {
		t.Fatalf("FailStaleJobs = %d, %v; want 1", n, err)
	}
	if got := payloadOf(t, db, "running"); got != "" {
		t.Errorf("interrupted job kept its payload: %q", got)
	}

	// A job that never started can still run, so it keeps its payload.
	if got := payloadOf(t, db, "queued"); got == "" {
		t.Error("queued job lost its payload and can no longer run")
	}
	active, err := db.ListActiveJobs()
	if err != nil || len(active) != 1 || active[0].ID != "queued" {
		t.Errorf("active jobs after restart = %+v, %v", active, err)
	}
}

func TestWipeFinishedJobPayloads(t *testing.T) {
	db := openTestDB(t)
	newJob(t, db, "old")
	newJob(t, db, "live")
	// Simulate a row finished by an older build that left the payload behind.
	if _, err := db.sql.Exec(`UPDATE jobs SET status = 'done' WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}

	n, err := db.WipeFinishedJobPayloads()
	if err != nil || n != 1 {
		t.Fatalf("wiped %d, %v; want 1", n, err)
	}
	if payloadOf(t, db, "old") != "" {
		t.Error("finished job payload survived")
	}
	if payloadOf(t, db, "live") == "" {
		t.Error("a queued job's payload must be kept")
	}
}
