package sso

import (
	"strings"
	"testing"
)

func TestCryptoRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	encoded, err := EncryptSecret(key, []byte("client-secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if strings.Contains(encoded, "client-secret") {
		t.Fatal("secret is not encrypted")
	}

	plain, err := DecryptSecret(key, encoded)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plain) != "client-secret" {
		t.Errorf("round trip = %q, want the secret back", plain)
	}

	if _, err := DecryptSecret(make([]byte, 32), encoded); err == nil {
		t.Error("decrypt with the wrong key should fail")
	}
	if _, err := EncryptSecret(make([]byte, 16), []byte("x")); err == nil {
		t.Error("encrypt with a short key should fail")
	}
}

func TestStateStoreSingleUse(t *testing.T) {
	store := newStateStore()

	state, err := store.save(7, "/instances", "nonce", "verifier", 42)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok := store.consume(state)
	if !ok {
		t.Fatal("fresh state should consume")
	}
	if got.providerID != 7 || got.next != "/instances" || got.nonce != "nonce" || got.verifier != "verifier" || got.linkUserID != 42 {
		t.Errorf("state = %+v, want the stored values", got)
	}

	if _, ok := store.consume(state); ok {
		t.Error("state must be single-use")
	}
	if _, ok := store.consume("bogus"); ok {
		t.Error("unknown state must not consume")
	}
}

func TestMethodPolicy(t *testing.T) {
	if !PasswordAllowed("") || !SSOAllowed("") {
		t.Error("empty method must allow both paths")
	}
	if !PasswordAllowed("either") || !SSOAllowed("either") {
		t.Error("either must allow both paths")
	}
	if !PasswordAllowed("password") || SSOAllowed("password") {
		t.Error("password-only must deny SSO")
	}
	if PasswordAllowed("sso") || !SSOAllowed("sso") {
		t.Error("sso-only must deny passwords")
	}
	if ValidMethod("root") {
		t.Error("unknown method must be rejected")
	}
}

func TestMFAClaimed(t *testing.T) {
	if !mfaClaimed([]string{"pwd", "mfa"}) {
		t.Error("mfa marker should count")
	}
	if !mfaClaimed([]string{"OTP"}) {
		t.Error("matching should ignore case")
	}
	if mfaClaimed([]string{"pwd"}) {
		t.Error("password alone is not multi-factor")
	}
	if mfaClaimed(nil) {
		t.Error("a missing amr claim is not multi-factor")
	}
}

func TestSuggestUsername(t *testing.T) {
	taken := map[string]bool{"jane-doe": true}
	exists := func(name string) bool { return taken[name] }

	if got := SuggestUsername(exists, "John Doe"); got != "john-doe" {
		t.Errorf("SuggestUsername = %q, want john-doe", got)
	}
	if got := SuggestUsername(exists, "Jane Doe"); got != "jane-doe-2" {
		t.Errorf("SuggestUsername = %q, want jane-doe-2", got)
	}
	if got := SuggestUsername(exists, "ops@Example.COM"); got != "ops-example.com" {
		t.Errorf("SuggestUsername = %q, want ops-example.com", got)
	}
	if got := SuggestUsername(exists, "!!"); got != "sso-user" {
		t.Errorf("SuggestUsername = %q, want sso-user", got)
	}
}

func TestLoginName(t *testing.T) {
	claims := idClaims{PreferredUsername: "jane", Email: "jane@example.com", Subject: "sub-1"}
	if got := loginName(claims); got != "jane" {
		t.Errorf("loginName = %q, want the preferred username", got)
	}
	claims.PreferredUsername = ""
	claims.Name = "Jane Doe"
	if got := loginName(claims); got != "Jane Doe" {
		t.Errorf("loginName = %q, want the name fallback", got)
	}
}

func TestNormalizeDomains(t *testing.T) {
	got, err := NormalizeDomains(" @Example.com, example.org;EXAMPLE.com\nfoo-bar.io ")
	if err != nil || got != "example.com,example.org,foo-bar.io" {
		t.Fatalf("NormalizeDomains = %q, %v", got, err)
	}
	if got, err := NormalizeDomains(""); got != "" || err != nil {
		t.Fatalf("empty = %q, %v", got, err)
	}
	for _, bad := range []string{"localhost", "a b@c.com", "exa_mple.com", ".com", "example.com/"} {
		if _, err := NormalizeDomains(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
