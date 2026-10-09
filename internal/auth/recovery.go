package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// recoveryCodeCount is how many codes one set holds.
	recoveryCodeCount = 8
	// recoveryCodeLen is the number of characters, without separators. 16
	// characters of a 31-letter alphabet is about 79 bits, enough that a
	// leaked hash cannot be brute-forced offline.
	recoveryCodeLen = 16
	// No 0/1/i/l/o: codes get read from paper.
	recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
)

// NormalizeRecoveryCode lowercases a code and removes the separators people
// type or paste. It returns "" when the result cannot be a recovery code.
func NormalizeRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(code) {
		switch {
		case r == '-' || r == ' ':
			continue
		case strings.ContainsRune(recoveryAlphabet, r):
			b.WriteRune(r)
		default:
			return ""
		}
	}
	if b.Len() != recoveryCodeLen {
		return ""
	}
	return b.String()
}

func hashRecoveryCode(normalized string) string {
	sum := sha256.Sum256([]byte("recovery:" + normalized))
	return hex.EncodeToString(sum[:])
}

func newRecoveryCode() (string, error) {
	// 248 is the largest multiple of the alphabet size that fits in a byte;
	// bytes above it are skipped so every letter is equally likely.
	limit := byte(256 - 256%len(recoveryAlphabet))
	out := make([]byte, 0, recoveryCodeLen)
	buf := make([]byte, 32)
	for len(out) < recoveryCodeLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, v := range buf {
			if v >= limit {
				continue
			}
			out = append(out, recoveryAlphabet[int(v)%len(recoveryAlphabet)])
			if len(out) == recoveryCodeLen {
				break
			}
		}
	}
	s := string(out)
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16], nil
}

// GenerateRecoveryCodes replaces the user's recovery codes with a fresh set
// and returns them. This is the only time they exist in readable form.
func (s *Service) GenerateRecoveryCodes(userID int64) ([]string, error) {
	codes := make([]string, 0, recoveryCodeCount)
	hashes := make([]string, 0, recoveryCodeCount)
	for len(codes) < recoveryCodeCount {
		c, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, c)
		hashes = append(hashes, hashRecoveryCode(NormalizeRecoveryCode(c)))
	}
	if err := s.db.ReplaceRecoveryCodes(userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// RecoveryCodesLeft counts the user's unused codes.
func (s *Service) RecoveryCodesLeft(userID int64) int {
	n, err := s.db.CountRecoveryCodes(userID)
	if err != nil {
		return 0
	}
	return n
}

// redeemRecoveryCode consumes a code if it matches an unused one.
func (s *Service) redeemRecoveryCode(userID int64, code string) bool {
	normalized := NormalizeRecoveryCode(code)
	if normalized == "" {
		return false
	}
	ok, err := s.db.ConsumeRecoveryCode(userID, hashRecoveryCode(normalized))
	if err != nil {
		s.log.Warn("could not redeem a recovery code", "err", err)
		return false
	}
	return ok
}
