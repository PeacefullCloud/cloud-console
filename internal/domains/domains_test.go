package domains

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/peaceful/cloud-console/internal/caddy"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
)

func TestValidDomain(t *testing.T) {
	good := []string{"example.com", "a.b.example.org", "xn--bcher-kva.example", "my-site.io", "1.example.com"}
	bad := []string{
		"", "a.b", "localhost", "-bad.example.com", "bad-.example.com", "a..example.com",
		"Example.com", "exa mple.com", "example.com/path", "example.com:8080",
		"a\nexample.com", "ex_ample.com", "*.example.com",
		"example.com; reverse_proxy evil",
		"{env.SECRET}.example.com",
	}
	for _, d := range good {
		if !validDomain(d) {
			t.Errorf("rejected %q", d)
		}
	}
	for _, d := range bad {
		if validDomain(d) {
			t.Errorf("accepted %q", d)
		}
	}
}

func TestAddRefusesTheConsoleHostAndInvalidNames(t *testing.T) {
	cfg := &config.Config{BaseURL: "https://console.example.com", CaddyConfigPath: "/tmp/x"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := &core.App{Cfg: cfg, Log: log, Caddy: caddy.New(cfg, log)}
	s := New(app)

	for _, domain := range []string{"console.example.com", "https://Console.Example.com/", "nope"} {
		err := s.Add(context.Background(), "web", domain, 80)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Add(%q) = %v, want ErrInvalid", domain, err)
		}
	}
}
