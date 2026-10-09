package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/peaceful/cloud-console/internal/models"
)

// totpIssuer is shown as the account issuer in authenticator apps.
const totpIssuer = "Peaceful Cloud"

// totpChallengeTTL bounds the second login step: after verifying the
// password, the user has this long to supply a code.
const totpChallengeTTL = 5 * time.Minute

// totpChallengeAttempts is how many wrong codes one challenge tolerates
// before the user has to enter the password again.
const totpChallengeAttempts = 5

// totpPeriod is the standard 30-second authenticator step.
const totpPeriod = 30

// Errors from second-factor verification.
var (
	ErrInvalidCode        = errors.New("incorrect code")
	ErrChallengeExhausted = errors.New("too many incorrect codes — sign in again")
	ErrChallengeExpired   = errors.New("that sign-in expired")
)

// totpChallenge is a pending second factor: the password was correct, but no
// session exists yet until a valid code arrives.
type totpChallenge struct {
	userID   int64
	expires  time.Time
	attempts int
}

// totpChallenges owns the pending second-factor logins. The console is a
// single process with an in-process job pool already, so in-memory state with
// expiry is consistent with the rest of the design.
var totpChallenges = struct {
	sync.Mutex
	items map[string]totpChallenge
}{items: map[string]totpChallenge{}}

// GenerateTOTPSecret creates a fresh secret for username and returns it along
// with the otpauth URL the user enters into their authenticator app.
func GenerateTOTPSecret(username string) (secret, otpURL string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: username,
	})
	if err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

// matchTOTPStep returns the time step whose code equals the submitted one,
// checking the previous, current and next 30-second windows to tolerate clock
// skew. Every window is compared, in constant time, regardless of an earlier
// match.
func matchTOTPStep(secret, code string, now time.Time) (step int64, ok bool) {
	if secret == "" || len(code) != 6 {
		return 0, false
	}

	opts := totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
	for offset := -1; offset <= 1; offset++ {
		at := now.Add(time.Duration(offset*totpPeriod) * time.Second)
		want, err := totp.GenerateCodeCustom(secret, at, opts)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			step, ok = at.Unix()/totpPeriod, true
		}
	}
	return step, ok
}

// VerifyTOTPCode checks a 6-digit code against the secret. It is stateless,
// so it suits confirming a fresh enrolment; the sign-in path uses
// VerifyLoginTOTP, which also blocks replays and counts failures.
func VerifyTOTPCode(secret, code string) bool {
	_, ok := matchTOTPStep(secret, code, time.Now())
	return ok
}

// ProvisioningURL rebuilds the otpauth URL for an already-stored secret so
// the settings page can show it again during setup.
func ProvisioningURL(username, secret string) string {
	if secret == "" {
		return ""
	}
	label := url.PathEscape(totpIssuer + ":" + username)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", totpIssuer)
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// newTOTPChallenge records a passed password check and returns the challenge
// id the verification form posts back.
func newTOTPChallenge(userID int64) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)

	totpChallenges.Lock()
	defer totpChallenges.Unlock()

	now := time.Now()
	for key, ch := range totpChallenges.items {
		if now.After(ch.expires) {
			delete(totpChallenges.items, key)
		}
	}
	totpChallenges.items[id] = totpChallenge{userID: userID, expires: now.Add(totpChallengeTTL)}
	return id, nil
}

// BeginTOTPChallenge records a passed password check and returns the id the
// code form posts back. The challenge expires quickly and survives wrong codes
// only up to totpChallengeAttempts. A user whose recent codes were wrong too
// often gets a *LockoutError instead, so signing in again cannot reset the
// budget.
func (s *Service) BeginTOTPChallenge(userID int64) (string, error) {
	if blocked, wait := s.totpThrottle.Blocked(userKey(userID)); blocked {
		return "", &LockoutError{RetryAfter: wait}
	}
	return newTOTPChallenge(userID)
}

// PeekTOTPChallenge resolves a challenge id without consuming it, so the
// code form can live at its own URL.
func (s *Service) PeekTOTPChallenge(id string) (int64, bool) {
	totpChallenges.Lock()
	defer totpChallenges.Unlock()

	ch, ok := totpChallenges.items[id]
	if !ok || s.clock().After(ch.expires) {
		return 0, false
	}
	return ch.userID, true
}

// VerifyLoginTOTP checks the second-factor code for a pending login.
//
// A correct code consumes the challenge. A wrong code keeps it, counts against
// both the challenge and the user's rolling budget, and ends it once either
// runs out. A code that was already redeemed is rejected even inside its
// validity window.
//
// Errors are ErrChallengeExpired, ErrInvalidCode, ErrChallengeExhausted, or a
// *LockoutError.
func (s *Service) VerifyLoginTOTP(challengeID string, userID int64, secret string, enabled bool, code string) error {
	_, err := s.VerifySecondFactor(challengeID, userID, secret, enabled, code)
	return err
}

// VerifySecondFactor is VerifyLoginTOTP that also accepts a recovery code in
// place of the authenticator code. usedRecovery reports which one matched. A
// recovery code counts against the same attempt budgets as a wrong TOTP code.
func (s *Service) VerifySecondFactor(challengeID string, userID int64, secret string, enabled bool, code string) (usedRecovery bool, err error) {
	key := userKey(userID)

	totpChallenges.Lock()
	ch, ok := totpChallenges.items[challengeID]
	if !ok || ch.userID != userID || s.clock().After(ch.expires) {
		delete(totpChallenges.items, challengeID)
		totpChallenges.Unlock()
		return false, ErrChallengeExpired
	}
	if blocked, wait := s.totpThrottle.Blocked(key); blocked {
		delete(totpChallenges.items, challengeID)
		totpChallenges.Unlock()
		return false, &LockoutError{RetryAfter: wait}
	}

	step, matched := matchTOTPStep(secret, code, s.clock())
	if matched && enabled && s.claimStep(userID, step) {
		delete(totpChallenges.items, challengeID)
		totpChallenges.Unlock()
		s.totpThrottle.Reset(key)
		return false, nil
	}

	// A recovery code stands in for the app. It is redeemed inside the same
	// lock and budget as a TOTP code, so guessing them is no easier.
	if enabled && NormalizeRecoveryCode(code) != "" && s.redeemRecoveryCode(userID, code) {
		delete(totpChallenges.items, challengeID)
		totpChallenges.Unlock()
		s.totpThrottle.Reset(key)
		return true, nil
	}

	s.totpThrottle.Fail(key)
	ch.attempts++
	exhausted := ch.attempts >= totpChallengeAttempts
	if exhausted {
		delete(totpChallenges.items, challengeID)
	} else {
		totpChallenges.items[challengeID] = ch
	}
	totpChallenges.Unlock()

	if blocked, wait := s.totpThrottle.Blocked(key); blocked {
		return false, &LockoutError{RetryAfter: wait}
	}
	if exhausted {
		return false, ErrChallengeExhausted
	}
	return false, ErrInvalidCode
}

// claimStep records step as redeemed and reports whether it was new. Steps at
// or before the last redeemed one are replays.
func (s *Service) claimStep(userID, step int64) bool {
	s.stepMu.Lock()
	defer s.stepMu.Unlock()

	if step <= s.lastStep[userID] {
		return false
	}
	s.lastStep[userID] = step
	return true
}

func userKey(userID int64) string {
	return "user:" + strconv.FormatInt(userID, 10)
}

// OpenSession creates a session for an already-authenticated user (used after
// the second factor succeeds).
func (s *Service) OpenSession(user *models.User, ip, userAgent string) (string, error) {
	token, tokenHash, err := NewToken()
	if err != nil {
		return "", err
	}

	expires := time.Now().Add(time.Duration(s.cfg.SessionTTL) * time.Hour)
	if err := s.db.CreateSession(tokenHash, user.ID, expires, ip, userAgent); err != nil {
		return "", err
	}
	if err := s.db.TouchLastLogin(user.ID); err != nil {
		s.log.Warn("could not update last login", "err", err)
	}
	return token, nil
}
