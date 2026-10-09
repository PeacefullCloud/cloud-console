package caddy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/peaceful/cloud-console/internal/config"
)

func newManager(cfg *config.Config) *Manager {
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRenderSkipsDuplicatesAndConsoleHost(t *testing.T) {
	m := newManager(&config.Config{BaseURL: "https://Console.example.com", Addr: "127.0.0.1:8080", CaddyConfigPath: "/x"})
	out := m.Render([]Route{
		{Domain: "a.example.com", Upstream: "10.0.0.2:80"},
		{Domain: "a.example.com", Upstream: "10.0.0.3:80"},
		{Domain: "console.example.com", Upstream: "10.0.0.4:80"},
		{Domain: "", Upstream: "10.0.0.5:80"},
	})
	if n := strings.Count(out, "a.example.com {"); n != 1 {
		t.Errorf("a.example.com appears %d times", n)
	}
	if n := strings.Count(out, "console.example.com {"); n != 1 {
		t.Errorf("console host appears %d times", n)
	}
	if strings.Contains(out, "10.0.0.4") {
		t.Error("an instance took over the console host")
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	m := newManager(&config.Config{CaddyConfigPath: "/x"})
	a := m.Render([]Route{{"b.test", "1:80"}, {"a.test", "2:80"}})
	b := m.Render([]Route{{"a.test", "2:80"}, {"b.test", "1:80"}})
	if a != b {
		t.Error("route order changes the output")
	}
}

func TestSyncWritesFileAfterAdminAPIAccepts(t *testing.T) {
	var loads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loads.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "Caddyfile")
	m := newManager(&config.Config{CaddyConfigPath: path, CaddyAdminURL: srv.URL})
	routes := []Route{{"a.test", "10.0.0.2:80"}}

	if err := m.Sync(context.Background(), routes); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "a.test {") {
		t.Fatalf("caddyfile not written: %q", body)
	}

	// Nothing changed: no second reload.
	if err := m.Sync(context.Background(), routes); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 1 {
		t.Errorf("reloaded %d times, want 1", loads.Load())
	}
}

func TestSyncKeepsGoodFileWhenAdminAPIRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad config", http.StatusBadRequest)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(path, []byte("known good\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newManager(&config.Config{CaddyConfigPath: path, CaddyAdminURL: srv.URL})

	if err := m.Sync(context.Background(), []Route{{"a.test", "10.0.0.2:80"}}); err == nil {
		t.Fatal("expected the rejection to be reported")
	}
	if body, _ := os.ReadFile(path); string(body) != "known good\n" {
		t.Errorf("a rejected config replaced the live file: %q", body)
	}
	if m.last != "" {
		t.Error("a failed sync was remembered as applied")
	}
}

func TestSyncRestoresFileWhenReloadCommandFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(path, []byte("known good\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newManager(&config.Config{CaddyConfigPath: path, CaddyReloadCmd: "false"})

	if err := m.Sync(context.Background(), []Route{{"a.test", "10.0.0.2:80"}}); err == nil {
		t.Fatal("expected the failing reload to be reported")
	}
	if body, _ := os.ReadFile(path); string(body) != "known good\n" {
		t.Errorf("previous caddyfile not restored: %q", body)
	}
}

func TestSyncIsSerialised(t *testing.T) {
	var active, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		active.Add(-1)
	}))
	defer srv.Close()

	m := newManager(&config.Config{CaddyAdminURL: srv.URL})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.Sync(context.Background(), []Route{{"a" + string(rune('a'+i)) + ".test", "1:80"}})
		}(i)
	}
	wg.Wait()
	if peak.Load() > 1 {
		t.Errorf("%d reloads ran at once", peak.Load())
	}
}

func TestSyncDisabledIsNoop(t *testing.T) {
	if err := newManager(&config.Config{}).Sync(context.Background(), []Route{{"a.test", "1:80"}}); err != nil {
		t.Fatal(err)
	}
}

func TestHostFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://console.example.com":        "console.example.com",
		"https://console.example.com:8443/x": "console.example.com",
		"console.example.com/path":           "console.example.com",
		"":                                   "",
	} {
		if got := hostFromURL(in); got != want {
			t.Errorf("hostFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
