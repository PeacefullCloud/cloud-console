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
}

// New creates the auth service.
func New(db *database.DB, cfg *config.Config, log *slog.Logger) *Service {
	return &Service{db: db, cfg: cfg, log: log}
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

// Login validates credentials and opens a session.
func (s *Service) Login(username, password, ip, userAgent string) (token string, user *models.User, err error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", nil, ErrInvalidCredentials
	}

	user, err = s.db.GetUserByUsername(username)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			// Spend the same time as a real check to avoid user enumeration.
			_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidin"), []byte(password))
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, err
	}

	if !VerifyPassword(user.PasswordHash, password) {
		return "", nil, ErrInvalidCredentials
	}

	token, tokenHash, err := NewToken()
	if err != nil {
		return "", nil, err
	}

	expires := time.Now().Add(time.Duration(s.cfg.SessionTTL) * time.Hour)
	if err := s.db.CreateSession(tokenHash, user.ID, expires, ip, userAgent); err != nil {
		return "", nil, err
	}
	if err := s.db.TouchLastLogin(user.ID); err != nil {
		s.log.Warn("could not update last login", "err", err)
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
