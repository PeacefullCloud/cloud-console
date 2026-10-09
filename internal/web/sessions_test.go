package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/activity"
	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/sso"
)

func sessionTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.App.Cfg.SessionTTL = 24
	s.App.Auth = auth.New(s.App.DB, s.App.Cfg, s.log)
	s.App.Activity = activity.New(s.App.DB, s.log)
	s.SSO = sso.New(s.App.DB, s.App.Cfg, s.log)
	s.Renderer = newTestRenderer(t)
	return s
}

func mustUser(t *testing.T, s *Server, name, role string) *models.User {
	t.Helper()
	hash, err := auth.HashPassword("old password 1")
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.App.DB.CreateUser(name, hash, role)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func formRequest(user *models.User, token string, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := context.WithValue(r.Context(), userKey{}, user)
	if token != "" {
		ctx = context.WithValue(ctx, tokenKey{}, token)
	}
	return r.WithContext(ctx)
}

func TestAdminResetsAnotherUsersPassword(t *testing.T) {
	s := sessionTestServer(t)
	admin := mustUser(t, s, "root", auth.RoleAdmin)
	victim := mustUser(t, s, "vic", auth.RoleViewer)
	tok, err := s.App.Auth.OpenSession(victim, "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}

	r := formRequest(admin, "", url.Values{"new_password": {"brand new pass"}})
	r.SetPathValue("id", strconv.FormatInt(victim.ID, 10))
	s.handleUserPasswordReset(httptest.NewRecorder(), r)

	if _, err := s.App.Auth.Authenticate(tok); err == nil {
		t.Error("old session survived a password reset")
	}
	got, _ := s.App.DB.GetUser(victim.ID)
	if !auth.VerifyPassword(got.PasswordHash, "brand new pass") {
		t.Error("new password was not applied")
	}
}

func TestPasswordResetIsAdminOnlyAndValidated(t *testing.T) {
	s := sessionTestServer(t)
	admin := mustUser(t, s, "root", auth.RoleAdmin)
	op := mustUser(t, s, "op", auth.RoleOperator)
	victim := mustUser(t, s, "vic", auth.RoleViewer)

	try := func(actor *models.User, id int64, pw string) {
		r := formRequest(actor, "", url.Values{"new_password": {pw}})
		r.SetPathValue("id", strconv.FormatInt(id, 10))
		s.handleUserPasswordReset(httptest.NewRecorder(), r)
	}
	unchanged := func(u *models.User) bool {
		got, _ := s.App.DB.GetUser(u.ID)
		return auth.VerifyPassword(got.PasswordHash, "old password 1")
	}

	try(op, victim.ID, "operator override")
	if !unchanged(victim) {
		t.Error("an operator reset someone's password")
	}
	try(admin, victim.ID, "short")
	if !unchanged(victim) {
		t.Error("a too-short password was accepted")
	}
	try(admin, admin.ID, "my own new pass")
	if !unchanged(admin) {
		t.Error("admin reset their own password without the current one")
	}
}

func TestSessionListAndRevoke(t *testing.T) {
	s := sessionTestServer(t)
	me := mustUser(t, s, "me", auth.RoleOperator)
	other := mustUser(t, s, "other", auth.RoleOperator)

	cur, _ := s.App.Auth.OpenSession(me, "1.1.1.1", "Mozilla/5.0 (X11; Linux x86_64) Firefox/120.0")
	second, _ := s.App.Auth.OpenSession(me, "2.2.2.2", "Mozilla/5.0 (iPhone) Safari/604")
	theirs, _ := s.App.Auth.OpenSession(other, "3.3.3.3", "ua")

	r := formRequest(me, cur, nil)
	views := s.sessionViews(r, me.ID)
	if len(views) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(views))
	}
	currentCount := 0
	for _, v := range views {
		if v.Current {
			currentCount++
			if v.Device != "Firefox on Linux" {
				t.Errorf("device label = %q", v.Device)
			}
		}
	}
	if currentCount != 1 {
		t.Errorf("exactly one session should be current, got %d", currentCount)
	}

	// A user cannot revoke somebody else's session by handle.
	r = formRequest(me, cur, nil)
	r.SetPathValue("id", database.SessionHandle(auth.HashToken(theirs)))
	s.handleSessionRevoke(httptest.NewRecorder(), r)
	if _, err := s.App.Auth.Authenticate(theirs); err != nil {
		t.Error("revoked another user's session")
	}

	// Revoking the current session through this endpoint is refused.
	r = formRequest(me, cur, nil)
	r.SetPathValue("id", database.SessionHandle(auth.HashToken(cur)))
	s.handleSessionRevoke(httptest.NewRecorder(), r)
	if _, err := s.App.Auth.Authenticate(cur); err != nil {
		t.Error("current session was revoked")
	}

	// Revoking another of my own sessions works.
	r = formRequest(me, cur, nil)
	r.SetPathValue("id", database.SessionHandle(auth.HashToken(second)))
	s.handleSessionRevoke(httptest.NewRecorder(), r)
	if _, err := s.App.Auth.Authenticate(second); err == nil {
		t.Error("session was not revoked")
	}
}

func TestRevokeOtherSessionsKeepsCurrent(t *testing.T) {
	s := sessionTestServer(t)
	me := mustUser(t, s, "me", auth.RoleViewer)
	other := mustUser(t, s, "other", auth.RoleViewer)
	cur, _ := s.App.Auth.OpenSession(me, "", "")
	a, _ := s.App.Auth.OpenSession(me, "", "")
	b, _ := s.App.Auth.OpenSession(me, "", "")
	theirs, _ := s.App.Auth.OpenSession(other, "", "")

	s.handleSessionsRevokeOthers(httptest.NewRecorder(), formRequest(me, cur, nil))

	for name, tok := range map[string]string{"a": a, "b": b} {
		if _, err := s.App.Auth.Authenticate(tok); err == nil {
			t.Errorf("session %s survived", name)
		}
	}
	for name, tok := range map[string]string{"current": cur, "other user's": theirs} {
		if _, err := s.App.Auth.Authenticate(tok); err != nil {
			t.Errorf("%s session was revoked", name)
		}
	}
}

func TestSessionHandleShortInputRevokesNothing(t *testing.T) {
	s := sessionTestServer(t)
	me := mustUser(t, s, "me", auth.RoleViewer)
	_ = s.App.DB.CreateSession("abcdef0123456789zzzz", me.ID, time.Now().Add(time.Hour), "", "")
	for _, h := range []string{"", "a", "abcdef"} {
		ok, err := s.App.DB.DeleteUserSessionByHandle(me.ID, h)
		if err != nil || ok {
			t.Errorf("handle %q: ok=%v err=%v", h, ok, err)
		}
	}
}

func TestSettingsPageRendersSessionsAndReset(t *testing.T) {
	s := sessionTestServer(t)
	admin := mustUser(t, s, "root", auth.RoleAdmin)
	mustUser(t, s, "vic", auth.RoleViewer)
	tok, _ := s.App.Auth.OpenSession(admin, "9.9.9.9", "Mozilla/5.0 Chrome/1 Safari/1 Windows")
	_, _ = s.App.Auth.OpenSession(admin, "8.8.8.8", "Mozilla/5.0 Firefox/1 Linux")

	data := settingsWithUsers(admin)
	data.Sessions = s.sessionViews(formRequest(admin, tok, nil), admin.ID)
	users, _ := s.App.DB.ListUsers()
	data.Users = users

	var sb strings.Builder
	if err := newTestRenderer(t).Execute(&sb, "settings", data); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"Signed-in devices", "this device", "Chrome on Windows", "Sign out all other devices", "/password", "Reset password"} {
		if !strings.Contains(out, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}
