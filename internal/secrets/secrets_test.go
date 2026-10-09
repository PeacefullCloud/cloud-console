package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testSealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := New(DeriveKey("test key"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := testSealer(t)
	sealed, err := s.Seal("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(sealed) || strings.Contains(sealed, "JBSWY3DPEHPK3PXP") {
		t.Fatalf("value is not sealed: %q", sealed)
	}
	got, err := s.Open(sealed)
	if err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestSealIsRandomised(t *testing.T) {
	s := testSealer(t)
	a, _ := s.Seal("x")
	b, _ := s.Seal("x")
	if a == b {
		t.Fatal("same ciphertext twice: nonce is not random")
	}
}

func TestEmptyStaysEmpty(t *testing.T) {
	s := testSealer(t)
	if v, err := s.Seal(""); v != "" || err != nil {
		t.Fatalf("Seal(\"\") = %q, %v", v, err)
	}
	if v, err := s.Open(""); v != "" || err != nil {
		t.Fatalf("Open(\"\") = %q, %v", v, err)
	}
}

func TestOpenPassesLegacyPlaintextThrough(t *testing.T) {
	if v, err := testSealer(t).Open("JBSWY3DPEHPK3PXP"); err != nil || v != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("Open = %q, %v", v, err)
	}
}

func TestOpenRejectsWrongKeyAndTampering(t *testing.T) {
	s := testSealer(t)
	sealed, _ := s.Seal("secret")

	other, _ := New(DeriveKey("another key"))
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("opened with the wrong key")
	}
	if _, err := s.Open(sealed[:len(sealed)-4] + "AAAA"); err == nil {
		t.Fatal("opened a tampered value")
	}
	if _, err := s.Open(prefix + "!!!"); err == nil {
		t.Fatal("opened invalid base64")
	}
}

func TestLoadKeyCreatesAndReusesFile(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadKey("", dir)
	if err != nil || len(first) != 32 {
		t.Fatalf("LoadKey = %v, %v", first, err)
	}
	info, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v", info.Mode().Perm())
	}
	second, err := LoadKey("", dir)
	if err != nil || string(first) != string(second) {
		t.Fatalf("key changed between loads: %v", err)
	}
}

func TestLoadKeyPrefersConfiguredValue(t *testing.T) {
	dir := t.TempDir()
	key, err := LoadKey("passphrase", dir)
	if err != nil || len(key) != 32 {
		t.Fatalf("LoadKey = %v, %v", key, err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFile)); !os.IsNotExist(err) {
		t.Fatal("key file created although a key was configured")
	}
}

func TestLoadKeyRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyFile), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey("", dir); err == nil {
		t.Fatal("accepted a corrupt key file")
	}
}
