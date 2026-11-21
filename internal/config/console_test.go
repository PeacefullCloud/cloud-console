package config

import (
	"strings"
	"testing"
)

func loadWithTerminalEnv(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	for key, value := range env {
		t.Setenv(key, value)
	}
	t.Setenv("CONSOLE_DATA_DIR", t.TempDir())
	return Load()
}

func TestTerminalUserValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		user    string
		wantErr bool
	}{
		{"default", DefaultTerminalUser, false},
		{"underscore and digits", "cloud_user2", false},
		{"root refused", "root", true},
		{"spaces refused", "cloud user", true},
		{"shell metacharacters refused", "user;id", true},
		{"uppercase refused", "CloudUser", true},
		{"empty refused", "", false}, // empty means "use the default"
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadWithTerminalEnv(t, map[string]string{"CONSOLE_TERMINAL_LOGIN_USER": tc.user})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.user)
				}
				if !strings.Contains(err.Error(), "CONSOLE_TERMINAL_LOGIN_USER") {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := tc.user
			if want == "" {
				want = DefaultTerminalUser
			}
			if c.TerminalLoginUser != want {
				t.Errorf("login user = %q, want %q", c.TerminalLoginUser, want)
			}
		})
	}
}

func TestTerminalLimitsNormalised(t *testing.T) {
	c, err := loadWithTerminalEnv(t, map[string]string{
		"CONSOLE_TERMINAL_MAX_SESSIONS":         "2",
		"CONSOLE_TERMINAL_MAX_PER_USER":         "9",
		"CONSOLE_TERMINAL_MAX_INPUT_BYTES":      "16",
		"CONSOLE_TERMINAL_IDLE_TIMEOUT_SECONDS": "-5",
		"CONSOLE_TERMINAL_ALLOWED_HOSTS":        "cloud.example.com, proxy.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.TerminalMaxPerUser != 2 {
		t.Errorf("per-user cap = %d, want clamped to global cap 2", c.TerminalMaxPerUser)
	}
	if c.TerminalMaxInputBytes < 1024 {
		t.Errorf("input cap = %d, want at least 1024", c.TerminalMaxInputBytes)
	}
	if c.TerminalIdleTimeout != 0 {
		t.Errorf("idle timeout = %v, want 0", c.TerminalIdleTimeout)
	}
	if len(c.TerminalAllowedHosts) != 2 || c.TerminalAllowedHosts[0] != "cloud.example.com" {
		t.Errorf("allowed hosts = %v", c.TerminalAllowedHosts)
	}
}

func TestCsvList(t *testing.T) {
	got := csvList(" one, two ,,three ")
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("csvList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("csvList = %v, want %v", got, want)
		}
	}
}
