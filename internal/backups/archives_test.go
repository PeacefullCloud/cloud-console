package backups

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
)

func archiveService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	app := &core.App{
		Cfg: &config.Config{DataDir: dir},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &Service{app: app}, dir
}

func writeArchive(t *testing.T, dir, instance, name string) string {
	t.Helper()
	p := filepath.Join(dir, "backups", instance, name+".tar.gz")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRenameMovesArchivesWithTheInstance(t *testing.T) {
	s, dir := archiveService(t)
	writeArchive(t, dir, "old", "one")
	writeArchive(t, dir, "old", "two")

	s.InstanceRenamed("old", "new")

	for _, n := range []string{"one", "two"} {
		path, err := s.archivePath("new", n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("archive %s not found under the new name: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "backups", "old")); !os.IsNotExist(err) {
		t.Errorf("old directory still exists: %v", err)
	}
}

func TestRenameNeverOverwritesAnExistingArchive(t *testing.T) {
	s, dir := archiveService(t)
	writeArchive(t, dir, "old", "same")
	keep := writeArchive(t, dir, "new", "same")
	if err := os.WriteFile(keep, []byte("keep me"), 0o640); err != nil {
		t.Fatal(err)
	}

	s.InstanceRenamed("old", "new")

	if body, _ := os.ReadFile(keep); string(body) != "keep me" {
		t.Errorf("existing archive was overwritten: %q", body)
	}
}

func TestRenameWithoutArchivesIsHarmless(t *testing.T) {
	s, dir := archiveService(t)
	s.InstanceRenamed("old", "new")
	if _, err := os.Stat(filepath.Join(dir, "backups", "new")); !os.IsNotExist(err) {
		t.Errorf("created a directory for nothing: %v", err)
	}
}

func TestRenameRejectsUnsafeNames(t *testing.T) {
	s, dir := archiveService(t)
	writeArchive(t, dir, "old", "one")
	s.InstanceRenamed("old", "../escape")
	if _, err := os.Stat(filepath.Join(dir, "escape")); !os.IsNotExist(err) {
		t.Errorf("moved archives outside the backups directory: %v", err)
	}
}

func TestDeleteRemovesLocalArchives(t *testing.T) {
	s, dir := archiveService(t)
	writeArchive(t, dir, "gone", "one")
	other := writeArchive(t, dir, "stays", "one")

	s.InstanceDeleted(context.Background(), "gone")

	if _, err := os.Stat(filepath.Join(dir, "backups", "gone")); !os.IsNotExist(err) {
		t.Errorf("archives of the deleted instance remain: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another instance's archive was removed: %v", err)
	}
}

func TestDeleteRejectsUnsafeNames(t *testing.T) {
	s, dir := archiveService(t)
	keep := writeArchive(t, dir, "stays", "one")
	s.InstanceDeleted(context.Background(), "..")
	s.InstanceDeleted(context.Background(), "")
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("an unsafe name deleted real data: %v", err)
	}
}
