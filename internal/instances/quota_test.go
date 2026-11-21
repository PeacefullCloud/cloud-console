package instances

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
)

func TestCapacityRefusesOnlyImpossibleRequests(t *testing.T) {
	host := struct {
		cpus int
		mem  int64
	}{8, 16 << 30}

	cases := []struct {
		name string
		req  CreateRequest
		ok   bool
	}{
		{"fits", CreateRequest{CPU: 4, MemoryMB: 8192}, true},
		{"exactly the host", CreateRequest{CPU: 8, MemoryMB: 16384}, true},
		{"too many cpus", CreateRequest{CPU: 9, MemoryMB: 1024}, false},
		{"too much memory", CreateRequest{CPU: 1, MemoryMB: 16385}, false},
	}
	for _, tc := range cases {
		err := checkCapacity(tc.req, host.cpus, host.mem)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		if err != nil && !errors.Is(err, ErrLimitsTooBig) {
			t.Errorf("%s: wrong error type: %v", tc.name, err)
		}
	}

	// Unknown host figures must not block creation.
	if err := checkCapacity(CreateRequest{CPU: 64, MemoryMB: 1 << 20}, 0, 0); err != nil {
		t.Errorf("unknown host refused: %v", err)
	}
}

func TestQuotaDisabledWhenZero(t *testing.T) {
	s := New(&core.App{Cfg: &config.Config{MaxInstances: 0}})
	if err := s.checkQuota(context.Background(), 0); err != nil {
		t.Errorf("quota 0 should mean unlimited, got %v", err)
	}
}

// The per-owner quota counts tracked instances and creates still in the queue,
// without counting the same name twice while a create job is finishing up.
func TestOwnerQuotaCountsInstancesAndQueuedCreates(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := database.Open(filepath.Join(t.TempDir(), "console.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user, err := db.CreateUser("alice", "x", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserQuota(user.ID, 2); err != nil {
		t.Fatal(err)
	}

	s := New(&core.App{Cfg: &config.Config{}, DB: db, Log: log})

	if err := db.SaveInstanceMeta(&models.InstanceMeta{Name: "one", OwnerID: user.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.checkOwnerQuota(user.ID); err != nil {
		t.Fatalf("one instance under a quota of two was refused: %v", err)
	}

	// A queued create for the same name is the same future instance and must
	// not consume a second slot.
	if err := db.CreateJob(&models.Job{
		ID: "dup", Kind: JobCreate, Target: "one", Status: "queued", OwnerID: user.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.checkOwnerQuota(user.ID); err != nil {
		t.Fatalf("a duplicate queued create consumed a second slot: %v", err)
	}

	// A genuinely new queued instance does consume a slot.
	if err := db.CreateJob(&models.Job{
		ID: "second", Kind: JobCreate, Target: "two", Status: "queued", OwnerID: user.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.checkOwnerQuota(user.ID); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota error = %v, want ErrQuota", err)
	}

	// Finished jobs no longer count.
	if err := db.FinishJob("second", "failed", "Failed", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.checkOwnerQuota(user.ID); err != nil {
		t.Fatalf("a finished create still consumed a slot: %v", err)
	}

	// Other users' instances and jobs are not this owner's business.
	other, err := db.CreateUser("bob", "x", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserQuota(other.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveInstanceMeta(&models.InstanceMeta{Name: "bobs-box", OwnerID: other.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.checkOwnerQuota(user.ID); err != nil {
		t.Fatalf("another tenant's usage counted against the owner: %v", err)
	}
	if err := s.checkOwnerQuota(other.ID); !errors.Is(err, ErrQuota) {
		t.Fatalf("bob's own quota error = %v, want ErrQuota", err)
	}
}
