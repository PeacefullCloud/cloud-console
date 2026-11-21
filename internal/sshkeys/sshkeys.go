// Package sshkeys handles the cryptographic side of the console's SSH key
// vault: generating keypairs and validating/normalizing public keys.
//
// It never stores anything. Only public keys reach the database; a generated
// private key is returned to the caller for a single, one-time display.
package sshkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Generate creates a fresh Ed25519 keypair. public is a single authorized_keys
// line, private is an OpenSSH-format PEM block, and fingerprint is the SSH
// SHA256 fingerprint of the public key.
func Generate(comment string) (public, private, fingerprint string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", fmt.Errorf("generate ed25519 key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", "", fmt.Errorf("encode public key: %w", err)
	}

	c := cleanComment(comment)
	public = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if c != "" {
		public += " " + c
	}

	block, err := ssh.MarshalPrivateKey(priv, c)
	if err != nil {
		return "", "", "", fmt.Errorf("encode private key: %w", err)
	}
	private = string(pem.EncodeToMemory(block))

	return public, private, ssh.FingerprintSHA256(sshPub), nil
}

// Parse validates an authorized_keys value and returns its normalized single
// line and SHA256 fingerprint. More than one key is rejected so pasting a
// whole authorized_keys file cannot silently store only the first entry.
func Parse(value string) (normalized, fingerprint string, err error) {
	lines := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) != 1 {
		return "", "", fmt.Errorf("enter exactly one SSH public key")
	}

	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(lines[0]))
	if err != nil {
		return "", "", fmt.Errorf("invalid SSH public key: %w", err)
	}
	return lines[0], ssh.FingerprintSHA256(pub), nil
}

// cleanComment keeps a user-supplied label from injecting extra lines or
// control characters into an authorized_keys entry.
func cleanComment(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	s = strings.TrimSpace(b.String())
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
