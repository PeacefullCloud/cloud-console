package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIP is the address of the caller, as far as it can be trusted. It feeds
// the sign-in throttle, session records and the audit log.
func (s *Server) clientIP(r *http.Request) string {
	return resolveClientIP(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), s.App.Cfg.TrustedProxies)
}

// resolveClientIP returns the connecting peer, unless that peer is a trusted
// proxy. For a trusted proxy it walks X-Forwarded-For from the right, which is
// the end each proxy appends to, and returns the first address that is not
// itself a trusted proxy. The leftmost entry is client-controlled and is never
// used on its own.
func resolveClientIP(remoteAddr string, forwarded []string, trusted []netip.Prefix) string {
	peer := parseHost(remoteAddr)
	if !peer.IsValid() {
		return remoteAddr
	}
	if !isTrusted(peer, trusted) {
		return peer.String()
	}

	var chain []string
	for _, header := range forwarded {
		for _, part := range strings.Split(header, ",") {
			if part = strings.TrimSpace(part); part != "" {
				chain = append(chain, part)
			}
		}
	}

	for i := len(chain) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(chain[i])
		if err != nil {
			// A garbled hop means nothing further left can be trusted.
			break
		}
		addr = addr.Unmap()
		if !isTrusted(addr, trusted) {
			return addr.String()
		}
	}
	return peer.String()
}

func parseHost(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap().WithZone("")
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
