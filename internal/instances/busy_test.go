package instances

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
)

func TestDeleteAndRenameRefusedWhileAJobRuns(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := database.Open(filepath.Join(t.TempDir(), "c.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.CreateJob(&models.Job{
		ID: "j1", Kind: "backup.create", Target: "web", Status: "queued", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Incus is nil on purpose: the guard must trigger before it is touched.
	s := New(&core.App{Cfg: &config.Config{}, DB: db, Log: log})

	if err := s.Delete(context.Background(), "web"); !errors.Is(err, ErrBusy) {
		t.Errorf("Delete err = %v, want ErrBusy", err)
	}
	if err := s.Rename(context.Background(), "web", "web2"); !errors.Is(err, ErrBusy) {
		t.Errorf("Rename err = %v, want ErrBusy", err)
	}
}
