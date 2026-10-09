package auth

import (
	"os"
	"strings"
	"testing"
)

func TestStoreInitialPassword(t *testing.T) {
	dir := t.TempDir()

	path, err := StoreInitialPassword(dir, "admin", "s3cret-one")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}

	// A second run replaces the file, even if its mode was loosened.
	_ = os.Chmod(path, 0o644)
	path, err = StoreInitialPassword(dir, "admin", "s3cret-two")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "s3cret-two") || strings.Contains(string(body), "s3cret-one") {
		t.Errorf("unexpected contents %q", body)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("replacement mode = %o, want 600", info.Mode().Perm())
	}
}
