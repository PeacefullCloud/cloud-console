// Package secrets seals small values (such as TOTP seeds) before they are
// written to the database.
//
// Sealed values carry a version prefix, so values stored before encryption
// existed can still be read and are upgraded on startup.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	prefix = "enc:v1:"

	// KeyFile is the generated key kept in the data directory when no key is
	// supplied through the environment.
	KeyFile = "secret.key"
)

// Sealer encrypts and decrypts values with AES-256-GCM.
type Sealer struct {
	gcm cipher.AEAD
}

// New returns a Sealer for a 32-byte key.
func New(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("secret key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{gcm: gcm}, nil
}

// IsSealed reports whether v was produced by Seal.
func IsSealed(v string) bool { return strings.HasPrefix(v, prefix) }

// Seal encrypts plain. The empty string stays empty so "no secret" is not
// turned into a ciphertext.
func (s *Sealer) Seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.gcm.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a sealed value. Values without the version prefix are
// returned unchanged: they predate encryption.
func (s *Sealer) Open(v string) (string, error) {
	if !IsSealed(v) {
		return v, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, prefix))
	if err != nil {
		return "", fmt.Errorf("decode sealed value: %w", err)
	}
	if len(raw) < s.gcm.NonceSize() {
		return "", errors.New("sealed value is too short")
	}
	nonce, body := raw[:s.gcm.NonceSize()], raw[s.gcm.NonceSize():]
	plain, err := s.gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", errors.New("cannot decrypt stored secret: wrong or changed CONSOLE_SECRET_KEY / secret.key?")
	}
	return string(plain), nil
}

// DeriveKey turns a configured value into a 32-byte key: 64 hex characters are
// used directly, anything else is hashed with SHA-256.
func DeriveKey(raw string) []byte {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// LoadKey returns the key to use: the configured value when set, otherwise
// the key file in dataDir, which is created (0600) on first use.
func LoadKey(configured, dataDir string) ([]byte, error) {
	if key := DeriveKey(configured); key != nil {
		return key, nil
	}

	path := filepath.Join(dataDir, KeyFile)
	if body, err := os.ReadFile(path); err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(body)))
		if derr != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is not a 64-character hex key", path)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// O_EXCL: two processes starting together must not overwrite each
	// other's key and orphan whatever the first one already sealed.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return LoadKey("", dataDir)
		}
		return nil, err
	}
	_, werr := f.WriteString(hex.EncodeToString(key) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return nil, werr
	}
	return key, nil
}
