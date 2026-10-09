package sso

import (
	"errors"
	"fmt"
	"strings"
)

// Account policies for identities that are not linked to a console user yet.
const (
	// ProvisionOpen creates an account for anyone the provider authenticates.
	ProvisionOpen = "open"
	// ProvisionDomains creates one only for a verified email in AllowedDomains.
	ProvisionDomains = "domains"
	// ProvisionLinkOnly never creates accounts: users link from Settings.
	ProvisionLinkOnly = "link_only"
)

// ErrNotProvisioned means the provider does not let this identity create an
// account.
var ErrNotProvisioned = errors.New("no account exists for this sign-in — ask an administrator to add you or link your account")

// NormalizeDomains lowercases and de-duplicates a comma, space or newline
// separated list of email domains and returns it comma-separated.
func NormalizeDomains(raw string) (string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f), "@"))
		if d == "" || seen[d] {
			continue
		}
		if !validDomain(d) {
			return "", fmt.Errorf("%q is not a valid email domain", f)
		}
		seen[d] = true
		out = append(out, d)
	}
	return strings.Join(out, ","), nil
}

func validDomain(d string) bool {
	if len(d) > 253 || !strings.Contains(d, ".") || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return false
	}
	for _, r := range d {
		ok := r == '-' || r == '.' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// MayProvision reports whether an unlinked identity may get a new account.
// Unknown or empty policies refuse, so a bad value can never open sign-up.
func (l *Login) MayProvision() error {
	switch l.Provisioning {
	case ProvisionOpen:
		return nil
	case ProvisionDomains:
		// An unverified address is whatever the user typed at the provider.
		if !l.EmailVerified {
			return ErrNotProvisioned
		}
		at := strings.LastIndex(l.Email, "@")
		if at < 1 {
			return ErrNotProvisioned
		}
		domain := strings.ToLower(l.Email[at+1:])
		for _, allowed := range strings.Split(l.AllowedDomains, ",") {
			if allowed != "" && allowed == domain {
				return nil
			}
		}
	}
	return ErrNotProvisioned
}
