package auth

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/database"
)

func recoveryService(t *testing.T) (*Service, *database.DB, int64, *fakeClock) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := database.Open(filepath.Join(t.TempDir(), "c.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u, err := db.CreateUser("alice", "hash", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Unix(1_700_000_010, 0)}
	s := New(db, &config.Config{}, log)
	s.clock = clock.Now
	return s, db, u.ID, clock
}

func TestRecoveryCodesFormatAndStorage(t *testing.T) {
	s, db, uid, _ := recoveryService(t)
	codes, err := s.GenerateRecoveryCodes(uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 8 {
		t.Fatalf("%d codes", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 19 || strings.Count(c, "-") != 3 || NormalizeRecoveryCode(c) == "" {
			t.Errorf("bad code %q", c)
		}
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
	}
	if s.RecoveryCodesLeft(uid) != 8 {
		t.Errorf("left = %d", s.RecoveryCodesLeft(uid))
	}

	// Only hashes are stored.
	var stored int
	rows, _ := db.ListUsers()
	_ = rows
	for _, c := range codes {
		if ok, _ := db.ConsumeRecoveryCode(uid, strings.ReplaceAll(c, "-", "")); ok {
			stored++
		}
	}
	if stored != 0 {
		t.Error("a readable code matched the stored value: codes are not hashed")
	}
}

func TestNormalizeRecoveryCode(t *testing.T) {
	good := "abcd-efgh-jkmn-pqrs"
	if got := NormalizeRecoveryCode("  ABCD EFGH-jkmn-PQRS "); got != "abcdefghjkmnpqrs" {
		t.Errorf("got %q", got)
	}
	if NormalizeRecoveryCode(good) != "abcdefghjkmnpqrs" {
		t.Error("canonical form rejected")
	}
	for _, bad := range []string{"", "123456", "abcd-efgh-jkmn-pqr", "abcd-efgh-jkmn-pqrst", "abcd-efgh-jkmn-pqr0", "abcd-efgh-jkmn-pqr!"} {
		if NormalizeRecoveryCode(bad) != "" {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRecoveryCodeSignsInOnce(t *testing.T) {
	s, _, uid, _ := recoveryService(t)
	secret := testTOTPSecret
	codes, _ := s.GenerateRecoveryCodes(uid)

	ch, _ := s.BeginTOTPChallenge(uid)
	used, err := s.VerifySecondFactor(ch, uid, secret, true, strings.ToUpper(codes[0]))
	if err != nil || !used {
		t.Fatalf("recovery sign-in = %v, %v", used, err)
	}
	if s.RecoveryCodesLeft(uid) != 7 {
		t.Errorf("left = %d", s.RecoveryCodesLeft(uid))
	}

	// The same code never works twice.
	ch2, _ := s.BeginTOTPChallenge(uid)
	if _, err := s.VerifySecondFactor(ch2, uid, secret, true, codes[0]); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("reused code err = %v", err)
	}
	// Another code from the set still does.
	if used, err := s.VerifySecondFactor(ch2, uid, secret, true, codes[1]); err != nil || !used {
		t.Errorf("second code = %v, %v", used, err)
	}
}

func TestRecoveryCodeIgnoredWhenTwoFactorIsOffOrWrongUser(t *testing.T) {
	s, _, uid, _ := recoveryService(t)
	secret := testTOTPSecret
	codes, _ := s.GenerateRecoveryCodes(uid)

	ch, _ := s.BeginTOTPChallenge(uid)
	if _, err := s.VerifySecondFactor(ch, uid, secret, false, codes[0]); err == nil {
		t.Error("recovery code accepted while two-factor is off")
	}
	ch, _ = s.BeginTOTPChallenge(uid)
	if _, err := s.VerifySecondFactor(ch, uid+1, secret, true, codes[0]); err == nil {
		t.Error("challenge of another user accepted")
	}
}

func TestWrongRecoveryCodesAreThrottledLikeTOTP(t *testing.T) {
	s, _, uid, _ := recoveryService(t)
	secret := testTOTPSecret
	_, _ = s.GenerateRecoveryCodes(uid)

	var last error
	for i := 0; i < totpFailureLimit; i++ {
		ch, err := s.BeginTOTPChallenge(uid)
		if err != nil {
			last = err
			break
		}
		_, last = s.VerifySecondFactor(ch, uid, secret, true, "aaaa-aaaa-aaaa-aaaa")
	}
	var lock *LockoutError
	if !errors.As(last, &lock) {
		t.Errorf("repeated wrong recovery codes never locked the user out: %v", last)
	}
}

func TestRegeneratingInvalidatesTheOldSet(t *testing.T) {
	s, _, uid, _ := recoveryService(t)
	secret := testTOTPSecret
	old, _ := s.GenerateRecoveryCodes(uid)
	_, _ = s.GenerateRecoveryCodes(uid)

	ch, _ := s.BeginTOTPChallenge(uid)
	if _, err := s.VerifySecondFactor(ch, uid, secret, true, old[0]); err == nil {
		t.Error("an old code still works after regenerating")
	}
}

func TestClearingTwoFactorDeletesRecoveryCodes(t *testing.T) {
	s, db, uid, _ := recoveryService(t)
	_, _ = s.GenerateRecoveryCodes(uid)
	if err := db.ClearTOTP(uid); err != nil {
		t.Fatal(err)
	}
	if s.RecoveryCodesLeft(uid) != 0 {
		t.Error("recovery codes survived clearing two-factor")
	}
}
