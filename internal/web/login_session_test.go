package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/activity"
	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/sso"
)

func newLoginServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.App.Cfg.SessionTTL = 24
	s.App.Auth = auth.New(s.App.DB, s.App.Cfg, s.log)
	s.App.Activity = activity.New(s.App.DB, s.log)
	s.Renderer = newTestRenderer(t)
	return s
}

func submitLogin(s *Server, username, password string) *httptest.ResponseRecorder {
	form := url.Values{"username": {username}, "password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.RemoteAddr = "203.0.113.4:5000"
	w := httptest.NewRecorder()
	s.handleLoginSubmit(w, r)
	return w
}

func sessionCount(t *testing.T, s *Server) int {
	t.Helper()
	n, err := s.App.DB.CountSessions()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A correct password is not a sign-in until every requirement is met, so no
// session may exist before then.
func TestPasswordStepCreatesNoSessionForTwoFactorUsers(t *testing.T) {
	s := newLoginServer(t)
	hash, _ := auth.HashPassword("correct horse battery")
	user, err := s.App.DB.CreateUser("jane", hash, auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.App.DB.SetTOTPSecret(user.ID, "JBSWY3DPEHPK3PXP")
	_ = s.App.DB.SetTOTPEnabled(user.ID, true)

	w := submitLogin(s, "jane", "correct horse battery")
	if loc := w.Header().Get("HX-Redirect"); !strings.HasPrefix(loc, "/login/totp?challenge=") {
		t.Fatalf("expected the second-factor step, got HX-Redirect %q", loc)
	}
	if got := sessionCount(t, s); got != 0 {
		t.Fatalf("%d session(s) exist after the password step alone", got)
	}
}

func TestSSOOnlyAccountGetsNoSessionFromPassword(t *testing.T) {
	s := newLoginServer(t)
	hash, _ := auth.HashPassword("correct horse battery")
	user, err := s.App.DB.CreateUser("sso-user", hash, auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SetAuthMethod(user.ID, sso.MethodSSO); err != nil {
		t.Fatal(err)
	}

	w := submitLogin(s, "sso-user", "correct horse battery")
	if w.Header().Get("HX-Redirect") != "" {
		t.Fatal("an SSO-only account was signed in with a password")
	}
	if !strings.Contains(w.Body.String(), "single sign-on") {
		t.Error("the user should be told to use single sign-on")
	}
	if got := sessionCount(t, s); got != 0 {
		t.Fatalf("%d session(s) were created for an SSO-only account", got)
	}
}

func TestSSOOnlyTwoFactorAccountIsRefusedBeforeCodeStep(t *testing.T) {
	s := newLoginServer(t)
	hash, _ := auth.HashPassword("correct horse battery")
	user, _ := s.App.DB.CreateUser("both", hash, auth.RoleViewer)
	_ = s.App.DB.SetTOTPSecret(user.ID, "JBSWY3DPEHPK3PXP")
	_ = s.App.DB.SetTOTPEnabled(user.ID, true)
	_ = s.App.DB.SetAuthMethod(user.ID, sso.MethodSSO)

	w := submitLogin(s, "both", "correct horse battery")
	if loc := w.Header().Get("HX-Redirect"); loc != "" {
		t.Fatalf("should be refused, was sent to %q", loc)
	}
}

func TestPasswordLoginStillWorks(t *testing.T) {
	s := newLoginServer(t)
	hash, _ := auth.HashPassword("correct horse battery")
	_, _ = s.App.DB.CreateUser("plain", hash, auth.RoleViewer)

	w := submitLogin(s, "plain", "correct horse battery")
	if w.Header().Get("HX-Redirect") != "/" {
		t.Fatalf("HX-Redirect = %q, want /", w.Header().Get("HX-Redirect"))
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), sessionCookie+"=") {
		t.Error("no session cookie set")
	}
	if got := sessionCount(t, s); got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
}
