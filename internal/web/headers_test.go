package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func serveWithHeaders(t *testing.T, secure bool, path string) http.Header {
	t.Helper()
	s := newTestServer(t)
	s.App.Cfg.SecureCookies = secure
	h := s.withSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Header()
}

func TestSecurityHeaders(t *testing.T) {
	h := serveWithHeaders(t, false, "/instances")
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
		"Cache-Control":          "no-store",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe") {
		t.Error("scripts must not allow unsafe sources")
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be sent over plain HTTP")
	}
}

func TestSecurityHeadersSecureAndStatic(t *testing.T) {
	if got := serveWithHeaders(t, true, "/instances").Get("Strict-Transport-Security"); got == "" {
		t.Error("HSTS expected when cookies are secure")
	}
	if got := serveWithHeaders(t, false, "/static/app.js").Get("Cache-Control"); got != "" {
		t.Errorf("static assets keep their own caching, got %q", got)
	}
}

// The CSP forbids inline scripts and handlers, so no template may use them.
func TestTemplatesAreCSPClean(t *testing.T) {
	inlineHandler := regexp.MustCompile(`(?i)\s(on[a-z]+|hx-on[:a-z-]*)\s*=`)
	inlineScript := regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>`)
	jsURL := regexp.MustCompile(`(?i)(href|src|action)\s*=\s*["']\s*javascript:`)

	root := filepath.Join("..", "..", "templates")
	err := filepathWalk(root, func(path, body string) {
		if m := inlineHandler.FindString(body); m != "" {
			t.Errorf("%s: inline handler %q is blocked by the CSP", path, strings.TrimSpace(m))
		}
		for _, tag := range inlineScript.FindAllString(body, -1) {
			if !strings.Contains(tag, "src=") {
				t.Errorf("%s: inline <script> %q is blocked by the CSP", path, tag)
			}
		}
		if jsURL.MatchString(body) {
			t.Errorf("%s: javascript: URL is blocked by the CSP", path)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func filepathWalk(root string, fn func(path, body string)) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fn(path, string(body))
		return nil
	})
}
