package auth

import (
	"crypto/rand"
	"encoding/base32"
	"net/url"
	"sync"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/peaceful/cloud-console/internal/models"
)

// totpIssuer is shown as the account issuer in authenticator apps.
const totpIssuer = "Peaceful Cloud"

// totpChallengeTTL bounds the second login step: after verifying the
// password, the user has this long to supply a code.
const totpChallengeTTL = 5 * time.Minute

// totpChallenge is a pending second factor: the password was correct, but no
// session exists yet until a valid code arrives.
type totpChallenge struct {
	userID  int64
	expires time.Time
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

// VerifyTOTPCode checks a 6-digit code against the secret, accepting the
// previous and next 30-second windows to tolerate clock skew.
func VerifyTOTPCode(secret, code string) bool {
	if secret == "" || code == "" {
		return false
	}
	return totp.Validate(code, secret)
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

// consumeTOTPChallenge resolves a challenge to its user exactly once.
func consumeTOTPChallenge(id string) (int64, bool) {
	totpChallenges.Lock()
	defer totpChallenges.Unlock()

	ch, ok := totpChallenges.items[id]
	if !ok {
		return 0, false
	}
	delete(totpChallenges.items, id)
	if time.Now().After(ch.expires) {
		return 0, false
	}
	return ch.userID, true
}

// BeginTOTPChallenge records a passed password check and returns the id the
// code form posts back. The challenge is single-use and expires quickly.
func (s *Service) BeginTOTPChallenge(userID int64) (string, error) {
	return newTOTPChallenge(userID)
}

// FinishTOTPChallenge resolves a challenge id to its user exactly once.
func (s *Service) FinishTOTPChallenge(id string) (int64, bool) {
	return consumeTOTPChallenge(id)
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
