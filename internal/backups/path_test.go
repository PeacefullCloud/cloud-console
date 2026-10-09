package backups

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
)

func TestArchivePathStaysInsideBackupsDir(t *testing.T) {
	dir := t.TempDir()
	s := &Service{app: &core.App{Cfg: &config.Config{DataDir: dir}}}

	bad := [][2]string{
		{"web", "../../../etc/x"},
		{"web", "..%2F..%2Fx"},
		{"web", ".."},
		{"web", ".hidden"},
		{"web", "a/b"},
		{"web", `a\b`},
		{"web", ""},
		{"../other", "x"},
		{"..", "x"},
		{"a/b", "x"},
		{"", "x"},
		{"web", "bad\x00name"},
		{"web", strings.Repeat("a", 200)},
	}
	for _, c := range bad {
		if path, err := s.archivePath(c[0], c[1]); err == nil {
			t.Errorf("archivePath(%q, %q) = %q, want an error", c[0], c[1], path)
		}
		if got := s.displayPath(c[0], c[1]); got != "" {
			t.Errorf("displayPath(%q, %q) = %q, want empty", c[0], c[1], got)
		}
	}

	path, err := s.archivePath("web-1", "web-1-20240101-1200")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "backups", "web-1", "web-1-20240101-1200.tar.gz")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}
