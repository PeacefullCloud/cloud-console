package web

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/sso"
)

// newTestServer builds a Server backed by a real, temporary SQLite database.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := database.Open(filepath.Join(t.TempDir(), "console.db"), log)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	app := &core.App{Cfg: &config.Config{}, DB: db, Log: log}
	return &Server{Deps: Deps{App: app}, log: log}
}

// addProvider stores a provider row so identities can reference it (foreign
// keys are enforced) and returns its id.
func addProvider(t *testing.T, s *Server) int64 {
	t.Helper()
	p := &models.SSOProvider{
		Name: "Test IdP", Issuer: "https://idp.example.com", ClientID: "client",
		DefaultRole: auth.RoleViewer, Enabled: true,
	}
	if err := s.App.DB.SaveProvider(p); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	return p.ID
}

func TestSSOLoginNeverTakesOverExistingUser(t *testing.T) {
	s := newTestServer(t)
	pid := addProvider(t, s)

	hash, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := s.App.DB.CreateUser("admin", hash, auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	// An attacker who controls their display name at the provider chooses
	// "admin" and arrives with a subject nobody has linked yet.
	user, err := s.resolveSSOUser(&sso.Login{
		ProviderID:   pid,
		Subject:      "attacker-subject",
		Username:     "admin",
		Email:        "admin@example.com",
		DefaultRole:  auth.RoleViewer,
		Provisioning: sso.ProvisionOpen,
	})
	if err != nil {
		t.Fatalf("resolveSSOUser: %v", err)
	}

	if user.ID == admin.ID {
		t.Fatal("SSO login resolved to the existing admin account: account takeover")
	}
	if user.Username != "admin-2" {
		t.Errorf("new account username = %q, want a numbered variant admin-2", user.Username)
	}
	if user.Role != auth.RoleViewer {
		t.Errorf("new account role = %q, want viewer", user.Role)
	}
	if user.AuthMethod != sso.MethodSSO {
		t.Errorf("new account method = %q, want sso", user.AuthMethod)
	}
	if _, err := s.App.DB.FindIdentityUser(pid, "attacker-subject"); err != nil {
		t.Errorf("identity should be linked to the new account: %v", err)
	}
	if _, err := s.App.DB.FindIdentityUser(pid, "other"); err == nil {
		t.Error("an unrelated subject must not resolve")
	}

	// The admin account must have gained no identity.
	if s.userLinkedToProvider(admin.ID, pid) {
		t.Error("the admin account was linked to the attacker's identity")
	}
}

func TestSSOLoginReusesLinkedIdentity(t *testing.T) {
	s := newTestServer(t)
	pid := addProvider(t, s)

	hash, _ := auth.HashPassword("correct horse battery")
	jane, err := s.App.DB.CreateUser("jane", hash, auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.LinkIdentity(jane.ID, pid, "jane-subject"); err != nil {
		t.Fatal(err)
	}

	// The display name no longer matters once an identity is linked.
	user, err := s.resolveSSOUser(&sso.Login{ProviderID: pid, Subject: "jane-subject", Username: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != jane.ID {
		t.Errorf("linked identity resolved to user %d, want %d", user.ID, jane.ID)
	}
}

func TestSSOProvisioningHonoursDefaultRole(t *testing.T) {
	s := newTestServer(t)
	pid := addProvider(t, s)

	cases := []struct {
		role, want string
	}{
		{auth.RoleOperator, auth.RoleOperator},
		{auth.RoleAdmin, auth.RoleAdmin},
		{"", auth.RoleViewer},
		{"root", auth.RoleViewer},
	}
	for i, tc := range cases {
		user, err := s.resolveSSOUser(&sso.Login{
			ProviderID:   pid,
			Subject:      "subject-" + string(rune('a'+i)),
			Username:     "newcomer",
			DefaultRole:  tc.role,
			Provisioning: sso.ProvisionOpen,
		})
		if err != nil {
			t.Fatalf("role %q: %v", tc.role, err)
		}
		if user.Role != tc.want {
			t.Errorf("default role %q provisioned as %q, want %q", tc.role, user.Role, tc.want)
		}
	}
}

func TestSSOCallbackRejectsUnboundBrowser(t *testing.T) {
	s := newTestServer(t)

	for name, cookie := range map[string]*http.Cookie{
		"no cookie":    nil,
		"wrong cookie": {Name: ssoStateCookie, Value: "someone-elses-state"},
	} {
		req := httptest.NewRequest("GET", "/auth/sso/callback/1?code=abc&state=attackers-state", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		req.SetPathValue("provider", "1")
		rec := httptest.NewRecorder()

		// s.SSO is nil: reaching the provider exchange would panic, so a
		// clean redirect proves the binding check ran first.
		s.handleSSOCallback(rec, req)

		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s: got %d to %q, want a redirect to /login", name, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestSSOLinkOnlyProviderCreatesNoAccounts(t *testing.T) {
	s := newTestServer(t)
	pid := addProvider(t, s)

	for _, policy := range []string{sso.ProvisionLinkOnly, "", "bogus"} {
		_, err := s.resolveSSOUser(&sso.Login{
			ProviderID: pid, Subject: "stranger", Username: "stranger",
			Email: "x@example.com", EmailVerified: true, Provisioning: policy,
		})
		if !errors.Is(err, sso.ErrNotProvisioned) {
			t.Errorf("policy %q: err = %v, want ErrNotProvisioned", policy, err)
		}
	}
	if n, _ := s.App.DB.CountUsers(); n != 0 {
		t.Errorf("%d users created by a link-only provider", n)
	}
}

func TestSSODomainPolicy(t *testing.T) {
	cases := []struct {
		name     string
		email    string
		verified bool
		domains  string
		ok       bool
	}{
		{"matching verified", "a@example.com", true, "example.com", true},
		{"case-insensitive", "A@Example.COM", true, "example.com", true},
		{"unverified", "a@example.com", false, "example.com", false},
		{"other domain", "a@evil.com", true, "example.com", false},
		{"subdomain is not the domain", "a@sub.example.com", true, "example.com", false},
		{"suffix trick", "a@notexample.com", true, "example.com", false},
		{"second domain", "a@example.org", true, "example.com,example.org", true},
		{"no email", "", true, "example.com", false},
		{"empty list", "a@example.com", true, "", false},
	}
	for _, tc := range cases {
		l := &sso.Login{Provisioning: sso.ProvisionDomains, Email: tc.email, EmailVerified: tc.verified, AllowedDomains: tc.domains}
		if got := l.MayProvision() == nil; got != tc.ok {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.ok)
		}
	}
}
