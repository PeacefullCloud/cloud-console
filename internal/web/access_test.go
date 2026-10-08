package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/monitoring"
	"github.com/peaceful/cloud-console/internal/sso"
)

func requestAs(role *string) *http.Request {
	req := httptest.NewRequest("POST", "/instances", nil)
	if role == nil {
		return req
	}
	user := &models.User{ID: 7, Username: "tester", Role: *role}
	return req.WithContext(context.WithValue(req.Context(), userKey{}, user))
}

func strptr(s string) *string { return &s }

func TestCheckWriteAccess(t *testing.T) {
	for _, role := range []string{"admin", "operator"} {
		if err := checkWriteAccess(requestAs(strptr(role))); err != nil {
			t.Errorf("checkWriteAccess(%s) = %v, want nil", role, err)
		}
	}

	if err := checkWriteAccess(requestAs(strptr("viewer"))); err == nil {
		t.Error("checkWriteAccess(viewer) = nil, want an error")
	}
	if err := checkWriteAccess(requestAs(nil)); err == nil {
		t.Error("checkWriteAccess(anonymous) = nil, want an error")
	}
}

func TestCheckAdminAccess(t *testing.T) {
	if err := checkAdminAccess(requestAs(strptr("admin"))); err != nil {
		t.Errorf("checkAdminAccess(admin) = %v, want nil", err)
	}

	for _, role := range []string{"operator", "viewer"} {
		if err := checkAdminAccess(requestAs(strptr(role))); err == nil {
			t.Errorf("checkAdminAccess(%s) = nil, want an error", role)
		}
	}
	if err := checkAdminAccess(requestAs(nil)); err == nil {
		t.Error("checkAdminAccess(anonymous) = nil, want an error")
	}
}

// Viewers must see a read-only note instead of lifecycle buttons.
func TestInstanceActionsViewer(t *testing.T) {
	renderer := newTestRenderer(t)
	inst := sampleInstance()

	viewer := &models.User{ID: 2, Username: "viewer", Role: "viewer"}
	var buf bytes.Buffer
	data := instanceData{baseData: baseData{CSRF: "test-token", User: viewer}, Inst: &inst}
	if err := renderer.ExecutePartial(&buf, "instance", "instance_actions", data); err != nil {
		t.Fatalf("render instance actions: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "/state") || strings.Contains(out, "/delete") {
		t.Error("viewers must not see lifecycle actions")
	}
	if !strings.Contains(out, "read-only") {
		t.Error("viewers should see a read-only note")
	}
}

func settingsWithUsers(user *models.User) settingsData {
	return settingsData{
		baseData: baseData{
			Title: "Settings", Nav: "settings", CSRF: "test-token",
			User: user, Version: "test",
		},
		Host:  &monitoring.Host{},
		Users: []models.User{{ID: 1, Username: "admin", Role: "admin"}, {ID: 2, Username: "viewer", Role: "viewer"}},
	}
}

// The users fragment carries the rows, the flash and the count together.
func TestUsersFragments(t *testing.T) {
	renderer := newTestRenderer(t)
	admin := &models.User{ID: 1, Username: "admin", Role: "admin"}
	data := usersFragmentData{
		CSRF:   "test-token",
		User:   admin,
		Users:  []models.User{{ID: 1, Username: "admin", Role: "admin"}, {ID: 2, Username: "viewer", Role: "viewer"}},
		Notice: "User viewer deleted.",
	}

	var buf bytes.Buffer
	if err := renderer.ExecutePartial(&buf, "settings", "users_response", data); err != nil {
		t.Fatalf("render users response: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `id="users-list"`) {
		t.Error("users response should contain the users list")
	}
	if !strings.Contains(out, "hx-swap-oob") {
		t.Error("users response should carry out-of-band swaps")
	}
	if !strings.Contains(out, "/settings/users/2/delete") {
		t.Error("users response should contain the delete action")
	}
	if !strings.Contains(out, "User viewer deleted.") {
		t.Error("users response should contain the flash message")
	}
}

// The TOTP fragment renders every card state without a reload.
func TestTOTPFragments(t *testing.T) {
	renderer := newTestRenderer(t)

	states := []totpFragmentData{
		{TOTPEnabled: true, Notice: "Two-factor authentication is on."},
		{TOTPSetupSecret: "SECRET", TOTPSetupURL: "otpauth://x", TOTPSetupQR: "data:image/png;base64,x"},
		{Error: "incorrect code — try again"},
	}
	for i, data := range states {
		data.CSRF = "test-token"
		var buf bytes.Buffer
		if err := renderer.ExecutePartial(&buf, "settings", "totp_response", data); err != nil {
			t.Fatalf("render totp response %d: %v", i, err)
		}
		out := buf.String()
		if !strings.Contains(out, `id="totp-card"`) {
			t.Errorf("totp response %d should contain the totp card", i)
		}
		if !strings.Contains(out, "hx-post") {
			t.Errorf("totp response %d should submit via HTMX", i)
		}
	}
}

// Enabled providers appear as sign-in buttons; disabled ones stay hidden.
func TestLoginSSOButtons(t *testing.T) {
	renderer := newTestRenderer(t)

	providers := []sso.ProviderView{
		{ID: 1, Name: "Google", ButtonLabel: "Google", Enabled: true},
		{ID: 2, Name: "Retired", ButtonLabel: "Retired", Enabled: false},
	}

	var buf bytes.Buffer
	data := loginData{Title: "Sign in", CSRF: "token", Next: "/instances", Providers: providers}
	// loginData lives in the layout-free page; render it standalone.
	if err := renderer.ExecuteStandalone(&buf, "templates/login.html", "login_page", data); err != nil {
		t.Fatalf("render login: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "/auth/sso/start/1?next=%2Finstances") {
		t.Error("enabled provider should offer a sign-in button preserving next")
	}
	if strings.Contains(out, "/auth/sso/start/2") {
		t.Error("disabled provider must not offer a sign-in button")
	}

	buf.Reset()
	if err := renderer.ExecuteStandalone(&buf, "templates/login.html", "login_page", loginData{Title: "Sign in"}); err != nil {
		t.Fatalf("render login: %v", err)
	}
	if strings.Contains(buf.String(), "/auth/sso/start/") {
		t.Error("password-only deployments must not show SSO buttons")
	}
}

// The SSO fragment carries the provider list, callback URLs and flash.
func TestSSOFragments(t *testing.T) {
	renderer := newTestRenderer(t)

	admin := &models.User{ID: 1, Username: "admin", Role: "admin"}
	data := ssoFragmentData{
		CSRF:      "test-token",
		User:      admin,
		SSOKeySet: true,
		Providers: []sso.ProviderView{{
			ID: 3, Name: "Keycloak", Issuer: "https://id.example.com/realms/x",
			ClientID: "console", HasSecret: true, ButtonLabel: "Keycloak",
			DefaultRole: "viewer", RequireMFA: true, Enabled: true,
			CallbackURL: "https://console.example.com/auth/sso/callback/3",
		}},
		Notice: "Sign-in method Keycloak saved.",
	}

	var buf bytes.Buffer
	if err := renderer.ExecutePartial(&buf, "settings", "sso_response", data); err != nil {
		t.Fatalf("render sso response: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `id="sso-list-wrap"`) {
		t.Error("sso response should contain the provider list")
	}
	if !strings.Contains(out, "hx-swap-oob") {
		t.Error("sso response should carry out-of-band swaps")
	}
	if !strings.Contains(out, "https://console.example.com/auth/sso/callback/3") {
		t.Error("sso response should show the exact callback URL to register")
	}
	if !strings.Contains(out, "Keycloak saved.") {
		t.Error("sso response should contain the flash message")
	}
}

// Admins assign each account exactly one allowed sign-in path.
func TestUsersMethodControls(t *testing.T) {
	renderer := newTestRenderer(t)

	users := []models.User{
		{ID: 1, Username: "admin", Role: "admin", AuthMethod: "either"},
		{ID: 2, Username: "viewer", Role: "viewer", AuthMethod: "sso", TOTPEnabled: true},
	}
	links := map[int64][]string{2: {"Keycloak"}}

	t.Run("admin", func(t *testing.T) {
		var buf bytes.Buffer
		data := usersFragmentData{
			CSRF: "test-token", User: &users[0], Users: users, Links: links,
		}
		if err := renderer.ExecutePartial(&buf, "settings", "users_response", data); err != nil {
			t.Fatalf("render users response: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "/settings/users/2/method") {
			t.Error("admins should see the sign-in method control")
		}
		if !strings.Contains(out, "Single sign-on: Keycloak") {
			t.Error("linked providers should show on the account")
		}
		if !strings.Contains(out, "/settings/users/2/totp/clear") {
			t.Error("admins should see 2FA reset for other users with it on")
		}
		if strings.Contains(out, "/settings/users/1/totp/clear") {
			t.Error("admins must not see 2FA reset for their own account")
		}
	})

	t.Run("viewer", func(t *testing.T) {
		var buf bytes.Buffer
		data := usersFragmentData{
			CSRF: "test-token", User: &users[1], Users: users, Links: links,
		}
		if err := renderer.ExecutePartial(&buf, "settings", "users_response", data); err != nil {
			t.Fatalf("render users response: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "/settings/users/2/method") || strings.Contains(out, "totp/clear") {
			t.Error("viewers must not see sign-in or 2FA controls")
		}
	})
}

// Admins get a delete button in front of every other username, but never for
// their own row. Viewers get neither delete buttons nor the add-user form.
func TestSettingsUsersDeleteButtons(t *testing.T) {
	renderer := newTestRenderer(t)

	t.Run("admin", func(t *testing.T) {
		var buf bytes.Buffer
		data := settingsWithUsers(&models.User{ID: 1, Username: "admin", Role: "admin"})
		if err := renderer.Execute(&buf, "settings", data); err != nil {
			t.Fatalf("render settings: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "/settings/users/2/delete") {
			t.Error("admins should see a delete button for other users")
		}
		if strings.Contains(out, "/settings/users/1/delete") {
			t.Error("admins must not see a delete button for their own account")
		}
		if !strings.Contains(out, `action="/settings/users"`) {
			t.Error("admins should see the add-user form")
		}
	})

	t.Run("viewer", func(t *testing.T) {
		var buf bytes.Buffer
		data := settingsWithUsers(&models.User{ID: 2, Username: "viewer", Role: "viewer"})
		if err := renderer.Execute(&buf, "settings", data); err != nil {
			t.Fatalf("render settings: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "/settings/users/2/delete") || strings.Contains(out, "/settings/users/1/delete") {
			t.Error("viewers must not see user delete buttons")
		}
		if strings.Contains(out, `action="/settings/users"`) {
			t.Error("viewers must not see the add-user form")
		}
	})
}
