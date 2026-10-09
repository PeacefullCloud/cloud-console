package web

import (
	"errors"
	"testing"

	"github.com/peaceful/cloud-console/internal/auth"
)

func TestPasswordGuessingLocksAccountEvenForCorrectPassword(t *testing.T) {
	s := newTestServer(t)
	s.App.Auth = auth.New(s.App.DB, s.App.Cfg, s.log)

	hash, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.App.DB.CreateUser("admin", hash, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	// Rotate source addresses: the per-username limit must hold regardless.
	for i := 0; i < 8; i++ {
		ip := "198.51.100." + string(rune('1'+i))
		if _, _, err := s.App.Auth.Login("admin", "wrong", ip, "test"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: want invalid credentials, got %v", i, err)
		}
	}

	var lockout *auth.LockoutError
	_, _, err = s.App.Auth.Login("admin", "correct horse battery", "203.0.113.9", "test")
	if !errors.As(err, &lockout) {
		t.Fatalf("correct password during lockout must be refused with LockoutError, got %v", err)
	}

	if _, _, err := s.App.Auth.Login("someone-else", "wrong", "203.0.113.9", "test"); errors.As(err, &lockout) {
		t.Fatal("lockout leaked onto an unrelated username")
	}
}

func TestPasswordSuccessClearsUsernameFailures(t *testing.T) {
	s := newTestServer(t)
	s.App.Auth = auth.New(s.App.DB, s.App.Cfg, s.log)

	hash, _ := auth.HashPassword("correct horse battery")
	if _, err := s.App.DB.CreateUser("jane", hash, auth.RoleViewer); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 3; round++ {
		for i := 0; i < 5; i++ {
			_, _, _ = s.App.Auth.Login("jane", "wrong", "192.0.2.1", "test")
		}
		if _, _, err := s.App.Auth.Login("jane", "correct horse battery", "192.0.2.1", "test"); err != nil {
			t.Fatalf("round %d: legitimate login failed: %v", round, err)
		}
	}
}
