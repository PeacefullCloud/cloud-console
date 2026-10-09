package database

import (
	"time"

	"github.com/peaceful/cloud-console/internal/models"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/secrets"
)

func testSealer(t *testing.T, pass string) *secrets.Sealer {
	t.Helper()
	s, err := secrets.New(secrets.DeriveKey(pass))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rawSecret(t *testing.T, d *DB, id int64) string {
	t.Helper()
	var v string
	if err := d.sql.QueryRow(`SELECT totp_secret FROM users WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTOTPSecretIsEncryptedAtRest(t *testing.T) {
	d := openTestDB(t)
	if err := d.SetSealer(testSealer(t, "k")); err != nil {
		t.Fatal(err)
	}
	u, err := d.CreateUser("alice", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetTOTPSecret(u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}

	if raw := rawSecret(t, d, u.ID); strings.Contains(raw, "JBSWY3DPEHPK3PXP") || !secrets.IsSealed(raw) {
		t.Fatalf("stored value is not encrypted: %q", raw)
	}
	got, err := d.GetUser(u.ID)
	if err != nil || got.TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("GetUser = %+v, %v", got, err)
	}
	byName, err := d.GetUserByUsername("alice")
	if err != nil || byName.TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("GetUserByUsername = %+v, %v", byName, err)
	}
	all, err := d.ListUsers()
	if err != nil || len(all) != 1 || all[0].TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("ListUsers = %+v, %v", all, err)
	}
}

func TestLegacyPlaintextSecretsAreSealedOnStartup(t *testing.T) {
	d := openTestDB(t)
	u, _ := d.CreateUser("bob", "hash", "admin")
	if err := d.SetTOTPSecret(u.ID, "LEGACYSECRET"); err != nil {
		t.Fatal(err)
	}
	if rawSecret(t, d, u.ID) != "LEGACYSECRET" {
		t.Fatal("expected plaintext before a sealer is set")
	}

	if err := d.SetSealer(testSealer(t, "k")); err != nil {
		t.Fatal(err)
	}
	if raw := rawSecret(t, d, u.ID); !secrets.IsSealed(raw) {
		t.Fatalf("legacy secret still plaintext: %q", raw)
	}
	got, _ := d.GetUser(u.ID)
	if got.TOTPSecret != "LEGACYSECRET" {
		t.Fatalf("secret changed by migration: %q", got.TOTPSecret)
	}

	// A second pass must not double-encrypt.
	before := rawSecret(t, d, u.ID)
	if err := d.sealLegacySecrets(); err != nil {
		t.Fatal(err)
	}
	if rawSecret(t, d, u.ID) != before {
		t.Fatal("already-sealed value was rewritten")
	}
}

func TestWrongKeyIsReportedNotSilentlyIgnored(t *testing.T) {
	d := openTestDB(t)
	_ = d.SetSealer(testSealer(t, "one"))
	u, _ := d.CreateUser("carol", "hash", "admin")
	_ = d.SetTOTPSecret(u.ID, "SECRET")

	d.sealer = testSealer(t, "two")
	if _, err := d.GetUser(u.ID); err == nil {
		t.Fatal("GetUser succeeded with the wrong key")
	}
}

func TestClearedSecretStaysEmpty(t *testing.T) {
	d := openTestDB(t)
	_ = d.SetSealer(testSealer(t, "k"))
	u, _ := d.CreateUser("dave", "hash", "admin")
	_ = d.SetTOTPSecret(u.ID, "SECRET")
	_ = d.ClearTOTP(u.ID)
	got, err := d.GetUser(u.ID)
	if err != nil || got.TOTPSecret != "" {
		t.Fatalf("GetUser = %+v, %v", got, err)
	}
}

func TestInsertMetricsStoresTheWholeBatch(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	batch := []models.Metric{
		{InstanceName: "a", TS: now, CPUPct: 1, MemUsed: 10},
		{InstanceName: "b", TS: now, CPUPct: 2, MemUsed: 20},
		{InstanceName: "a", TS: now.Add(time.Second), CPUPct: 3, MemUsed: 30},
	}
	if err := d.InsertMetrics(batch); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertMetrics(nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}

	a, err := d.ListMetrics("a", now.Add(-time.Minute))
	if err != nil || len(a) != 2 {
		t.Fatalf("a = %d samples, %v", len(a), err)
	}
	b, _ := d.ListMetrics("b", now.Add(-time.Minute))
	if len(b) != 1 || b[0].MemUsed != 20 {
		t.Errorf("b = %+v", b)
	}
}
