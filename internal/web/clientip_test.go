package web

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/peaceful/cloud-console/internal/config"
)

func mustProxies(t *testing.T, raw string) []netip.Prefix {
	t.Helper()
	p, err := config.ParseTrustedProxies(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveClientIP(t *testing.T) {
	loopback := mustProxies(t, config.DefaultTrustedProxies)
	caddyNet := mustProxies(t, "127.0.0.1, 172.18.0.0/16")

	tests := []struct {
		name      string
		remote    string
		forwarded []string
		trusted   []netip.Prefix
		want      string
	}{
		{"direct client, no proxies trusted", "203.0.113.7:51000", nil, nil, "203.0.113.7"},
		{"spoofed header from an untrusted peer is ignored", "203.0.113.7:51000", []string{"1.2.3.4"}, loopback, "203.0.113.7"},
		{"spoofed header with no trusted proxies is ignored", "203.0.113.7:51000", []string{"1.2.3.4"}, nil, "203.0.113.7"},
		{"local caddy reports the real client", "127.0.0.1:40000", []string{"198.51.100.9"}, loopback, "198.51.100.9"},
		{"client-supplied left entry is not used", "127.0.0.1:40000", []string{"6.6.6.6, 198.51.100.9"}, loopback, "198.51.100.9"},
		{"multiple header lines are joined", "127.0.0.1:40000", []string{"6.6.6.6", "198.51.100.9"}, loopback, "198.51.100.9"},
		{"trusted hops on the right are skipped", "172.18.0.2:40000", []string{"198.51.100.9, 172.18.0.5"}, caddyNet, "198.51.100.9"},
		{"trusted proxy without a header falls back to the peer", "127.0.0.1:40000", nil, loopback, "127.0.0.1"},
		{"garbled hop stops the walk", "127.0.0.1:40000", []string{"198.51.100.9, not-an-ip"}, loopback, "127.0.0.1"},
		{"ipv6 peer", "[2001:db8::5]:443", nil, loopback, "2001:db8::5"},
		{"ipv6 loopback proxy", "[::1]:40000", []string{"2001:db8::9"}, loopback, "2001:db8::9"},
		{"v4-mapped loopback is still loopback", "[::ffff:127.0.0.1]:40000", []string{"198.51.100.9"}, loopback, "198.51.100.9"},
		{"remote without a port", "203.0.113.7", nil, nil, "203.0.113.7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveClientIP(tc.remote, tc.forwarded, tc.trusted); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServerClientIPUsesConfig(t *testing.T) {
	s := newTestServer(t)
	s.App.Cfg.TrustedProxies = mustProxies(t, config.DefaultTrustedProxies)

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	if got := s.clientIP(r); got != "198.51.100.9" {
		t.Errorf("got %q", got)
	}

	r.RemoteAddr = "203.0.113.50:1234"
	if got := s.clientIP(r); got != "203.0.113.50" {
		t.Errorf("untrusted peer: got %q", got)
	}
}
