package web

import (
	"net/http"
	"strings"
)

// contentSecurityPolicy allows only same-origin scripts, so injected markup
// cannot run code. Inline styles stay allowed because templates use style
// attributes and htmx injects an indicator stylesheet. form-action is left
// out on purpose: Chromium applies it to the redirect that follows a form
// post, which would block the hand-off to a single sign-on provider.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"frame-ancestors 'none'"

// withSecurityHeaders sets browser hardening headers on every response.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if s.App.Cfg.SecureCookies {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		// Pages show account and infrastructure data and must not be kept by
		// shared caches or the back-forward cache after sign-out.
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
