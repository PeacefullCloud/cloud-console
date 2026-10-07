// Package caddy renders the host Caddyfile from the console's domain table and
// asks Caddy to reload.
//
// Caddy terminates public HTTPS traffic and forwards each domain to the right
// container over the Incus bridge. Containers only ever hold private IPs.
package caddy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peaceful/cloud-console/internal/config"
)

// Route maps a public hostname to a container upstream.
type Route struct {
	Domain   string // e.g. example.com
	Upstream string // e.g. 10.10.0.21:80
}

// Manager owns the Caddyfile and reloads Caddy when it changes.
type Manager struct {
	cfg *config.Config
	log *slog.Logger

	last string // last rendered config, used to skip no-op reloads
}

// New creates a Caddy manager.
func New(cfg *config.Config, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, log: log}
}

// Enabled reports whether the console is configured to manage Caddy.
func (m *Manager) Enabled() bool { return m.cfg.CaddyConfigured() }

// Render produces the full Caddyfile for the given routes.
//
// Caddy's automatic HTTPS issues and renews certificates for every domain.
func (m *Manager) Render(routes []Route) string {
	// Stable ordering keeps the file (and therefore reloads) deterministic.
	sorted := make([]Route, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })

	var b strings.Builder
	b.WriteString("# Managed by Peaceful Cloud Console. Do not edit by hand.\n")
	b.WriteString("# Regenerated whenever instances or domains change.\n\n")

	b.WriteString("{\n")
	b.WriteString("\t# Console-wide defaults.\n")
	b.WriteString("\tservers {\n")
	b.WriteString("\t\ttrusted_proxies static private_ranges\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")

	// Optional block for the console itself.
	if host := m.cfg.BaseURL; host != "" {
		if h := hostFromURL(host); h != "" && h != "localhost" {
			fmt.Fprintf(&b, "\n# Console UI\n%s {\n\tencode zstd gzip\n\treverse_proxy %s\n}\n",
				h, consoleUpstream(m.cfg.Addr))
		}
	}

	for _, r := range sorted {
		if r.Domain == "" || r.Upstream == "" {
			continue
		}

		fmt.Fprintf(&b, "\n%s {\n", r.Domain)
		b.WriteString("\tencode zstd gzip\n")
		fmt.Fprintf(&b, "\treverse_proxy %s {\n", r.Upstream)
		b.WriteString("\t\theader_up Host {upstream_hostport}\n")
		b.WriteString("\t\theader_up X-Real-IP {remote_host}\n")
		b.WriteString("\t\theader_up X-Forwarded-For {remote_host}\n")
		b.WriteString("\t\theader_up X-Forwarded-Proto {scheme}\n")
		b.WriteString("\t}\n")
		fmt.Fprintf(&b, "\tlog {\n\t\toutput file %s\n\t}\n",
			filepath.Join("/var/log/caddy", r.Domain+".log"))
		b.WriteString("}\n")
	}

	return b.String()
}

// Sync renders the Caddyfile, writes it to disk and reloads Caddy.
//
// A no-op when nothing changed, so frequent calls stay cheap.
func (m *Manager) Sync(ctx context.Context, routes []Route) error {
	if !m.Enabled() {
		return nil
	}

	rendered := m.Render(routes)
	if rendered == m.last {
		return nil
	}

	// Caddy validates a bad config on reload; keep the old one until it passes.
	if m.cfg.CaddyConfigPath != "" {
		if err := writeFileAtomic(m.cfg.CaddyConfigPath, rendered); err != nil {
			return fmt.Errorf("write caddyfile: %w", err)
		}
	}

	if err := m.reload(ctx, rendered); err != nil {
		return err
	}

	m.last = rendered
	m.log.Info("caddy configuration applied", "domains", len(routes))
	return nil
}

// reload hands the new configuration to Caddy.
func (m *Manager) reload(ctx context.Context, rendered string) error {
	// Preferred path: Caddy's admin API accepts a Caddyfile directly.
	if m.cfg.CaddyAdminURL != "" {
		return m.reloadViaAPI(ctx, rendered)
	}

	cmdline := m.cfg.CaddyReloadCmd
	if cmdline == "" {
		if m.cfg.CaddyConfigPath == "" {
			return nil
		}
		cmdline = "caddy reload --config " + shellQuote(m.cfg.CaddyConfigPath)
	}

	parts := strings.Fields(cmdline)
	if len(parts) == 0 {
		return nil
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, parts[0], parts[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("caddy reload failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (m *Manager) reloadViaAPI(ctx context.Context, rendered string) error {
	url := strings.TrimSuffix(m.cfg.CaddyAdminURL, "/") + "/load"

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodPost, url, strings.NewReader(rendered))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("caddy admin api returned %s: %s", resp.Status, strings.TrimSpace(buf.String()))
	}
	return nil
}

// writeFileAtomic replaces a file without ever leaving a partial Caddyfile.
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".caddyfile-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func hostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		return h
	}
	return raw
}

func consoleUpstream(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
