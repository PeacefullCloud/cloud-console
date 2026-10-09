// Package auth handles console sign-in: password hashing, session tokens and
// role checks.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
)

// Roles understood by the console.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Errors returned to callers (and mapped to friendly UI messages).
var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrNoSession          = errors.New("not signed in")
	ErrForbidden          = errors.New("permission denied")
)

// Service authenticates console users.
type Service struct {
	db  *database.DB
	cfg *config.Config
	log *slog.Logger

	// Brute-force limits. Password guesses are counted per username and per
	// client address; wrong 2FA codes per user across every challenge, so
	// signing in again does not reset the budget.
	userThrottle *Throttle
	ipThrottle   *Throttle
	totpThrottle *Throttle
	// confirmThrottle limits password re-checks made from an open session
	// (turning two-factor on or off), which a stolen cookie could otherwise
	// use to guess the password without ever hitting the login throttle.
	confirmThrottle *Throttle

	// lastStep remembers the newest TOTP time step each user redeemed, so a
	// code that was observed once cannot be replayed within its window.
	stepMu   sync.Mutex
	lastStep map[int64]int64

	clock func() time.Time
}

// Failure budgets inside throttleWindow.
const (
	throttleWindow      = 15 * time.Minute
	userLoginLimit      = 8
	ipLoginLimit        = 40
	totpFailureLimit    = 6
	confirmFailureLimit = 5
)

// New creates the auth service.
func New(db *database.DB, cfg *config.Config, log *slog.Logger) *Service {
	return &Service{
		db:              db,
		cfg:             cfg,
		log:             log,
		userThrottle:    NewThrottle(userLoginLimit, throttleWindow),
		ipThrottle:      NewThrottle(ipLoginLimit, throttleWindow),
		totpThrottle:    NewThrottle(totpFailureLimit, throttleWindow),
		confirmThrottle: NewThrottle(confirmFailureLimit, throttleWindow),
		lastStep:        map[int64]int64{},
		clock:           time.Now,
	}
}

// CheckLoginAllowed refuses a password attempt before the password is even
// looked at, so a locked account cannot be probed. The error is a
// *LockoutError.
func (s *Service) CheckLoginAllowed(username, ip string) error {
	if blocked, wait := s.userThrottle.Blocked(username); blocked {
		return &LockoutError{RetryAfter: wait}
	}
	if blocked, wait := s.ipThrottle.Blocked(ip); blocked {
		return &LockoutError{RetryAfter: wait}
	}
	return nil
}

// RecordLoginFailure counts a rejected password against the username and the
// address. Unknown usernames count too, so lockouts reveal nothing.
func (s *Service) RecordLoginFailure(username, ip string) {
	s.userThrottle.Fail(username)
	s.ipThrottle.Fail(ip)
}

// RecordLoginSuccess clears the username's password failures. The address
// keeps its history: one good account must not launder guesses at others.
func (s *Service) RecordLoginSuccess(username string) {
	s.userThrottle.Reset(username)
}

// ConfirmPassword re-checks the signed-in user's password before a sensitive
// change. Wrong guesses are counted per user; the error is ErrInvalidCredentials
// or a *LockoutError.
func (s *Service) ConfirmPassword(userID int64, password string) error {
	key := userKey(userID)
	if blocked, wait := s.confirmThrottle.Blocked(key); blocked {
		return &LockoutError{RetryAfter: wait}
	}

	user, err := s.db.GetUser(userID)
	if err != nil {
		return err
	}
	if !VerifyPassword(user.PasswordHash, password) {
		s.confirmThrottle.Fail(key)
		return ErrInvalidCredentials
	}
	s.confirmThrottle.Reset(key)
	return nil
}

// HashPassword hashes a plaintext password with bcrypt.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// VerifyPassword checks a plaintext password against a hash.
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// EnsureAdmin creates the bootstrap administrator on first run.
//
// When CONSOLE_ADMIN_PASSWORD is unset a random password is generated and
// returned so it can be printed once at startup.
func (s *Service) EnsureAdmin() (generatedPassword string, err error) {
	count, err := s.db.CountUsers()
	if err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil
	}

	password := s.cfg.AdminPassword
	if password == "" {
		password, err = RandomPassword(16)
		if err != nil {
			return "", err
		}
		generatedPassword = password
	}

	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}

	username := s.cfg.AdminUser
	if username == "" {
		username = "admin"
	}

	if _, err := s.db.CreateUser(username, hash, RoleAdmin); err != nil {
		return "", err
	}

	s.log.Info("created bootstrap administrator", "username", username)
	return generatedPassword, nil
}

// VerifyCredentials checks a username and password and returns the user. It
// creates no session, so callers can apply further requirements (a second
// factor, the account's allowed sign-in methods) before one exists. Attempts
// are throttled; the error may be ErrInvalidCredentials or a *LockoutError.
func (s *Service) VerifyCredentials(username, password, ip string) (*models.User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, ErrInvalidCredentials
	}
	if err := s.CheckLoginAllowed(username, ip); err != nil {
		return nil, err
	}

	user, err := s.db.GetUserByUsername(username)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			// Spend the same time as a real check to avoid user enumeration.
			_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidin"), []byte(password))
			s.RecordLoginFailure(username, ip)
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	if !VerifyPassword(user.PasswordHash, password) {
		s.RecordLoginFailure(username, ip)
		return nil, ErrInvalidCredentials
	}
	s.RecordLoginSuccess(username)
	return user, nil
}

// Login validates credentials and opens a session.
func (s *Service) Login(username, password, ip, userAgent string) (token string, user *models.User, err error) {
	user, err = s.VerifyCredentials(username, password, ip)
	if err != nil {
		return "", nil, err
	}

	token, err = s.OpenSession(user, ip, userAgent)
	if err != nil {
		return "", nil, err
	}
	return token, user, nil
}

// Authenticate resolves a session token to its user.
func (s *Service) Authenticate(token string) (*models.User, error) {
	if token == "" {
		return nil, ErrNoSession
	}

	user, err := s.db.SessionUser(HashToken(token))
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil, ErrNoSession
		}
		return nil, err
	}
	return user, nil
}

// Logout closes a session.
func (s *Service) Logout(token string) error {
	if token == "" {
		return nil
	}
	return s.db.DeleteSession(HashToken(token))
}

// ChangePassword updates a user's password and invalidates other sessions.
func (s *Service) ChangePassword(userID int64, current, next string) error {
	user, err := s.db.GetUser(userID)
	if err != nil {
		return err
	}
	if !VerifyPassword(user.PasswordHash, current) {
		return ErrInvalidCredentials
	}
	if len(next) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}

	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.db.UpdatePassword(userID, hash); err != nil {
		return err
	}
	return s.db.DeleteUserSessions(userID)
}

// ResetPassword sets a new password for a user without knowing the old one.
// It is for administrators helping a locked-out user, and signs every session
// of that user out. Authorisation is the caller's job.
func (s *Service) ResetPassword(userID int64, next string) error {
	if len(next) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	if _, err := s.db.GetUser(userID); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.db.UpdatePassword(userID, hash); err != nil {
		return err
	}
	return s.db.DeleteUserSessions(userID)
}

// CanWrite reports whether a role may change infrastructure.
func CanWrite(role string) bool {
	return role == RoleAdmin || role == RoleOperator
}

// CanAdmin reports whether a role may change console settings and users.
func CanAdmin(role string) bool { return role == RoleAdmin }

// NewToken returns a fresh session token and its storage hash.
func NewToken() (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken hashes a session token for storage.
//
// Sessions are looked up by hash, so a database leak cannot be replayed as a
// live cookie.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RandomPassword generates a readable random password.
func RandomPassword(length int) (string, error) {
	if length < 12 {
		length = 12
	}
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}
