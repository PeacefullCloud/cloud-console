// Package domains manages the public hostnames served by the host Caddy.
//
// A domain row points at an instance; the console resolves the instance's private
// address and rewrites the Caddyfile so Caddy can reverse proxy to it with
// automatic HTTPS.
package domains

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/peaceful/cloud-console/internal/caddy"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// Errors surfaced to the UI.
var (
	ErrNotFound     = fmt.Errorf("domain not found")
	ErrInvalid      = fmt.Errorf("invalid domain")
	ErrInstanceDown = fmt.Errorf("the instance has no address yet")
)

// Record is a domain joined with its instance's live address.
type Record struct {
	ID           int64
	Domain       string
	InstanceName string
	Port         int
	Upstream     string
	Reachable    bool
	CreatedAt    time.Time
}

// Service manages domains and keeps Caddy in sync.
type Service struct {
	app *core.App
}

// New creates the domain service.
func New(app *core.App) *Service {
	return &Service{app: app}
}

// List returns every domain with its resolved upstream.
func (s *Service) List(ctx context.Context) ([]Record, error) {
	rows, err := s.app.DB.ListDomains()
	if err != nil {
		return nil, err
	}
	return s.enrich(ctx, rows), nil
}

// ListForInstance returns the domains attached to a single instance.
func (s *Service) ListForInstance(ctx context.Context, instanceName string) ([]Record, error) {
	rows, err := s.app.DB.ListDomainsForInstance(instanceName)
	if err != nil {
		return nil, err
	}
	return s.enrich(ctx, rows), nil
}

// Add attaches a hostname to an instance.
func (s *Service) Add(ctx context.Context, instanceName, domain string, port int) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimSuffix(domain, "/")

	if !validDomain(domain) {
		return fmt.Errorf("%w: %q is not a valid hostname", ErrInvalid, domain)
	}
	if host := s.app.Caddy.ConsoleHost(); host != "" && domain == host {
		return fmt.Errorf("%w: %s is the console's own address", ErrInvalid, domain)
	}
	if port <= 0 || port > 65535 {
		port = 80
	}

	if _, err := s.app.Incus.Instance(instanceName); err != nil {
		return fmt.Errorf("instance %s: %w", instanceName, ErrNotFound)
	}

	if _, err := s.app.DB.CreateDomain(instanceName, domain, port); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return fmt.Errorf("%w: %s is already in use", ErrInvalid, domain)
		}
		return err
	}

	// The first domain becomes the instance's primary hostname.
	if meta, err := s.app.DB.GetInstanceMeta(instanceName); err == nil && meta.PrimaryHost == "" {
		_ = s.app.DB.SetPrimaryHost(instanceName, domain)
	}

	return s.Sync(ctx)
}

// Delete removes a domain mapping.
func (s *Service) Delete(ctx context.Context, id int64) error {
	row, err := s.app.DB.GetDomain(id)
	if err != nil {
		return ErrNotFound
	}

	if err := s.app.DB.DeleteDomain(id); err != nil {
		return err
	}

	// Re-point the primary hostname at another remaining domain, if any.
	if remaining, err := s.app.DB.ListDomainsForInstance(row.InstanceName); err == nil {
		next := ""
		if len(remaining) > 0 {
			next = remaining[0].Domain
		}
		_ = s.app.DB.SetPrimaryHost(row.InstanceName, next)
	}

	return s.Sync(ctx)
}

// DeleteForInstance removes every domain attached to an instance.
func (s *Service) DeleteForInstance(ctx context.Context, instanceName string) error {
	if err := s.app.DB.DeleteDomainsForInstance(instanceName); err != nil {
		return err
	}
	return s.Sync(ctx)
}

// InstanceChanged implements instances.Notifier so routing is refreshed after
// any instance change.
func (s *Service) InstanceChanged(ctx context.Context, name string) {
	if err := s.Sync(ctx); err != nil {
		s.app.Log.Error("could not refresh caddy configuration", "instance", name, "err", err)
	}
}

// Routes builds the Caddy route table from the domain list and live instance
// addresses.
func (s *Service) Routes(ctx context.Context) ([]caddy.Route, error) {
	rows, err := s.app.DB.ListDomains()
	if err != nil {
		return nil, err
	}

	// A failed listing must abort the sync. Treating it as "no instance has an
	// address" would render an empty Caddyfile and take every site down.
	addresses, err := s.addresses(ctx)
	if err != nil {
		return nil, err
	}

	routes := make([]caddy.Route, 0, len(rows))
	for _, row := range rows {
		ip := addresses[row.InstanceName]
		if ip == "" {
			// The instance is stopped; leave it out rather than proxying nowhere.
			s.app.Log.Warn("domain has no upstream, skipping", "domain", row.Domain, "instance", row.InstanceName)
			continue
		}

		port := row.Port
		if port == 0 {
			port = 80
		}

		routes = append(routes, caddy.Route{
			Domain:   row.Domain,
			Upstream: fmt.Sprintf("%s:%d", ip, port),
		})
	}

	return routes, nil
}

// Sync regenerates the Caddyfile and reloads Caddy.
func (s *Service) Sync(ctx context.Context) error {
	if !s.app.Caddy.Enabled() {
		return nil
	}

	routes, err := s.Routes(ctx)
	if err != nil {
		return err
	}
	return s.app.Caddy.Sync(ctx, routes)
}

// CaddyEnabled reports whether the console manages Caddy.
func (s *Service) CaddyEnabled() bool { return s.app.Caddy.Enabled() }

// PreviewCaddyfile renders the configuration without applying it, so the UI can
// show what would be written.
func (s *Service) PreviewCaddyfile(ctx context.Context) string {
	routes, err := s.Routes(ctx)
	if err != nil {
		return "# could not build routes: " + err.Error()
	}
	return s.app.Caddy.Render(routes)
}

func (s *Service) enrich(ctx context.Context, rows []models.Domain) []Record {
	addresses, err := s.addresses(ctx)
	if err != nil {
		s.app.Log.Warn("could not resolve domain upstreams", "err", err)
	}

	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		ip := addresses[row.InstanceName]

		port := row.Port
		if port == 0 {
			port = 80
		}

		rec := Record{
			ID:           row.ID,
			Domain:       row.Domain,
			InstanceName: row.InstanceName,
			Port:         port,
			CreatedAt:    row.CreatedAt,
			Reachable:    ip != "",
		}
		if ip != "" {
			rec.Upstream = fmt.Sprintf("%s:%d", ip, port)
		}
		out = append(out, rec)
	}
	return out
}

// addresses maps every instance to its private IPv4 address from a single
// (shared, cached) listing, rather than one Incus query per domain.
func (s *Service) addresses(ctx context.Context) (map[string]string, error) {
	fulls, err := s.app.Incus.Instances(ctx)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	out := make(map[string]string, len(fulls))
	for i := range fulls {
		out[fulls[i].Name] = incus.InstanceIP(&fulls[i])
	}
	return out, nil
}

func validDomain(domain string) bool {
	if len(domain) < 4 || len(domain) > 253 {
		return false
	}
	if !strings.Contains(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
