// Package sso adds OpenID Connect single sign-on to the console.
//
// Providers are configured in the Settings UI and stored in the database;
// client secrets are encrypted at rest with the key from CONSOLE_SSO_KEY.
// Sign-in itself stays session-based: a verified IdP login opens the same
// session cookie as a password login, so the rest of the console (CSRF,
// roles, activity log) works unchanged.
package sso

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// EncryptSecret seals a client secret with AES-256-GCM. The output is
// base64(nonce | ciphertext).
func EncryptSecret(key, plaintext []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("sso encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptSecret opens a value produced by EncryptSecret.
func DecryptSecret(key []byte, encoded string) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("sso encryption key must be 32 bytes")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode secret: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("secret is too short")
	}

	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errors.New("cannot decrypt secret: wrong CONSOLE_SSO_KEY?")
	}
	return plain, nil
}
