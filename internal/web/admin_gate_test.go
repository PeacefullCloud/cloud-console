package web

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/sso"
)

func renderSettingsAs(t *testing.T, role string) string {
	t.Helper()
	user := &models.User{ID: 5, Username: "sam", Role: role}
	data := settingsWithUsers(user)
	// Even if a handler leaked these, the template must not print them to a
	// non-admin.
	data.S3Configured = true
	data.S3Bucket = "secret-bucket-name"
	data.S3AccessKeyMasked = "AKIA…MASK"
	data.CaddyPath = "/etc/caddy/private.caddy"
	data.CaddyPreview = "internal-route.example.com"
	data.SessionCount = 41

	var buf bytes.Buffer
	if err := newTestRenderer(t).Execute(&buf, "settings", data); err != nil {
		t.Fatalf("render settings: %v", err)
	}
	return buf.String()
}

func TestSettingsHidesAdminDataFromOtherRoles(t *testing.T) {
	adminOnly := []string{"secret-bucket-name", "AKIA…MASK", "/etc/caddy/private.caddy", "internal-route.example.com", "Console users", "/settings/users"}

	for _, role := range []string{"viewer", "operator"} {
		out := renderSettingsAs(t, role)
		for _, leaked := range adminOnly {
			if strings.Contains(out, leaked) {
				t.Errorf("%s can see %q on the settings page", role, leaked)
			}
		}
		for _, own := range []string{"Your password", "Two-factor authentication", "Sign out"} {
			if !strings.Contains(out, own) {
				t.Errorf("%s lost their own %q section", role, own)
			}
		}
	}

	out := renderSettingsAs(t, "admin")
	for _, want := range adminOnly[:5] {
		if !strings.Contains(out, want) {
			t.Errorf("admin should see %q", want)
		}
	}
}

func TestUsersFragmentRefusedRequestListsNobody(t *testing.T) {
	s := newTestServer(t)
	s.SSO = sso.New(s.App.DB, s.App.Cfg, s.log)
	hash := "x"
	if _, err := s.App.DB.CreateUser("secret-admin", hash, "admin"); err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{"viewer", "operator"} {
		rec := httptest.NewRecorder()
		s.Renderer = newTestRenderer(t)
		r := requestAs(strptr(role))
		s.serveUsersFragments(rec, r, errAdminOnly, "")
		if strings.Contains(rec.Body.String(), "secret-admin") {
			t.Errorf("%s was shown the user list after a refused request", role)
		}
	}
}
