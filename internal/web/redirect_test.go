package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSafeNextRejectsBrowserTricks(t *testing.T) {
	ok := []string{"/", "/instances", "/instances/web?tab=backups", "/settings?totp=setup"}
	for _, in := range ok {
		if got := safeNext(in); got != in {
			t.Errorf("safeNext(%q) = %q, want it kept", in, got)
		}
	}

	bad := []string{
		"", "evil.example", "//evil.example", "//evil.example/x", "http://evil.example",
		"https://evil.example/", `/\evil.example`, "/\t/evil.example", "/\n/evil.example",
		"/\r/evil.example", "javascript:alert(1)",
	}
	for _, in := range bad {
		if got := safeNext(in); got != "" {
			t.Errorf("safeNext(%q) = %q, want it rejected", in, got)
		}
	}
}

func TestRedirectBackStaysOnSite(t *testing.T) {
	s := newTestServer(t)

	tests := []struct {
		referer, want string
	}{
		{"http://console.example/instances/web?tab=backups", "/instances/web?tab=backups"},
		{"http://console.example//evil.example/x", "/fallback"},
		{"http://console.example", "/fallback"},
		{"http://other.example/instances", "/fallback"},
		{"", "/fallback"},
		{"/settings", "/settings"},
		// url.Parse escapes the backslash, so the browser sees a literal path.
		{`http://console.example/\evil.example`, "/%5Cevil.example"},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodPost, "http://console.example/x", nil)
		if tc.referer != "" {
			r.Header.Set("Referer", tc.referer)
		}
		w := httptest.NewRecorder()
		s.redirectBack(w, r, "/fallback")
		if got := w.Header().Get("Location"); got != tc.want {
			t.Errorf("Referer %q: redirected to %q, want %q", tc.referer, got, tc.want)
		}
	}
}
