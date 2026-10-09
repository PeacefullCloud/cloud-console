package auth

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/peaceful/cloud-console/internal/config"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func TestThrottleBlocksAndRecovers(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	th := NewThrottle(3, 10*time.Minute)
	th.now = clock.Now

	for i := 0; i < 3; i++ {
		if blocked, _ := th.Blocked("Alice"); blocked {
			t.Fatalf("blocked after %d failures", i)
		}
		th.Fail("Alice")
		clock.advance(time.Minute)
	}

	blocked, wait := th.Blocked("alice")
	if !blocked {
		t.Fatal("key (case-insensitive) should be blocked after 3 failures")
	}
	if wait <= 0 || wait > 10*time.Minute {
		t.Fatalf("unexpected wait %v", wait)
	}
	if blocked, _ := th.Blocked("bob"); blocked {
		t.Fatal("other keys must be unaffected")
	}

	clock.advance(wait + time.Second)
	if blocked, _ := th.Blocked("alice"); blocked {
		t.Fatal("block should lift once the oldest failure ages out")
	}
}

func TestThrottleReset(t *testing.T) {
	th := NewThrottle(1, time.Hour)
	th.Fail("a")
	if blocked, _ := th.Blocked("a"); !blocked {
		t.Fatal("expected block")
	}
	th.Reset("a")
	if blocked, _ := th.Blocked("a"); blocked {
		t.Fatal("reset should clear the block")
	}
}

func TestThrottleBoundsKeys(t *testing.T) {
	th := NewThrottle(3, time.Hour)
	for i := 0; i < maxThrottleKeys+500; i++ {
		th.Fail(string(rune('a'+i%26)) + time.Duration(i).String())
	}
	if len(th.failures) > maxThrottleKeys {
		t.Fatalf("table grew to %d keys", len(th.failures))
	}
}

func newTOTPService(clock *fakeClock) *Service {
	s := New(nil, &config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.clock = clock.Now
	return s
}

func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestVerifyLoginTOTPAcceptsOnceThenRejectsReplay(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_010, 0)}
	s := newTOTPService(clock)
	secret := testTOTPSecret
	code := codeAt(t, secret, clock.now)

	first, err := s.BeginTOTPChallenge(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyLoginTOTP(first, 1, secret, true, code); err != nil {
		t.Fatalf("valid code rejected: %v", err)
	}
	if _, ok := s.PeekTOTPChallenge(first); ok {
		t.Fatal("challenge must be consumed by success")
	}

	second, err := s.BeginTOTPChallenge(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyLoginTOTP(second, 1, secret, true, code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("replayed code should be rejected, got %v", err)
	}

	clock.advance(30 * time.Second)
	if err := s.VerifyLoginTOTP(second, 1, secret, true, codeAt(t, secret, clock.now)); err != nil {
		t.Fatalf("next-step code rejected: %v", err)
	}
}

func TestVerifyLoginTOTPChallengeAttemptCap(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_010, 0)}
	s := newTOTPService(clock)
	secret := testTOTPSecret
	good := codeAt(t, secret, clock.now)

	id, _ := s.BeginTOTPChallenge(7)
	for i := 0; i < totpChallengeAttempts-1; i++ {
		if err := s.VerifyLoginTOTP(id, 7, secret, true, "000000"); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("attempt %d: want ErrInvalidCode, got %v", i, err)
		}
		if _, ok := s.PeekTOTPChallenge(id); !ok {
			t.Fatalf("attempt %d: challenge should survive", i)
		}
	}
	if err := s.VerifyLoginTOTP(id, 7, secret, true, "000000"); !errors.Is(err, ErrChallengeExhausted) {
		t.Fatalf("want ErrChallengeExhausted, got %v", err)
	}
	if err := s.VerifyLoginTOTP(id, 7, secret, true, good); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("a dead challenge must not accept even a valid code, got %v", err)
	}
}

// testTOTPSecret is fixed so that tests submitting "000000" as a wrong code
// cannot, by chance, hit a valid code of a random secret.
const testTOTPSecret = "JBSWY3DPEHPK3PXP"

func TestVerifyLoginTOTPBudgetSurvivesNewChallenges(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_010, 0)}
	s := newTOTPService(clock)
	s.totpThrottle.now = clock.Now
	secret := testTOTPSecret

	// Burn one challenge's attempts, then start over as a password re-login
	// would: the user-wide budget must carry across.
	id, _ := s.BeginTOTPChallenge(9)
	for i := 0; i < totpChallengeAttempts; i++ {
		_ = s.VerifyLoginTOTP(id, 9, secret, true, "000000")
	}

	id, err := s.BeginTOTPChallenge(9)
	if err != nil {
		t.Fatalf("budget is %d, only %d used: %v", totpFailureLimit, totpChallengeAttempts, err)
	}
	var lockout *LockoutError
	if err := s.VerifyLoginTOTP(id, 9, secret, true, "000000"); !errors.As(err, &lockout) {
		t.Fatalf("failure number %d should lock the user, got %v", totpFailureLimit, err)
	}
	if _, err := s.BeginTOTPChallenge(9); !errors.As(err, &lockout) {
		t.Fatalf("new challenge must be refused while locked, got %v", err)
	}
	if _, err := s.BeginTOTPChallenge(10); err != nil {
		t.Fatalf("other users must be unaffected: %v", err)
	}

	clock.advance(throttleWindow + time.Minute)
	if _, err := s.BeginTOTPChallenge(9); err != nil {
		t.Fatalf("lock should lift after the window: %v", err)
	}
}

func TestVerifyLoginTOTPRejectsDisabledAndWrongUser(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_010, 0)}
	s := newTOTPService(clock)
	secret := testTOTPSecret
	code := codeAt(t, secret, clock.now)

	id, _ := s.BeginTOTPChallenge(1)
	if err := s.VerifyLoginTOTP(id, 2, secret, true, code); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("challenge bound to another user must fail, got %v", err)
	}

	id, _ = s.BeginTOTPChallenge(3)
	if err := s.VerifyLoginTOTP(id, 3, secret, false, code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("2FA-disabled account must not pass, got %v", err)
	}
}

func TestMatchTOTPStepRejectsMalformed(t *testing.T) {
	secret := testTOTPSecret
	for _, code := range []string{"", "12345", "1234567", "abcdef", " 123456"} {
		if _, ok := matchTOTPStep(secret, code, time.Now()); ok {
			t.Fatalf("%q accepted", code)
		}
	}
}
