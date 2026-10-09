package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/activity"
	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/models"
)

func postAs(t *testing.T, s *Server, user *models.User, h http.HandlerFunc, form url.Values) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/settings/totp", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
	h(httptest.NewRecorder(), r)
}

func newTOTPTestServer(t *testing.T) (*Server, *models.User) {
	t.Helper()
	s := newTestServer(t)
	s.App.Auth = auth.New(s.App.DB, s.App.Cfg, s.log)
	hash, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.App.DB.CreateUser("jane", hash, auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	s.App.Activity = activity.New(s.App.DB, s.log)
	return s, user
}

func TestTOTPSetupCannotDisableExistingTwoFactor(t *testing.T) {
	s, user := newTOTPTestServer(t)
	if err := s.App.DB.SetTOTPSecret(user.ID, "ORIGINALSECRET"); err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SetTOTPEnabled(user.ID, true); err != nil {
		t.Fatal(err)
	}

	// Even with the right password, setup must not replace a live secret.
	postAs(t, s, user, s.handleTOTPSetup, url.Values{"current_password": {"correct horse battery"}})

	got, _ := s.App.DB.GetUser(user.ID)
	if !got.TOTPEnabled || got.TOTPSecret != "ORIGINALSECRET" {
		t.Fatalf("two-factor was changed by a setup request: enabled=%v secret=%q", got.TOTPEnabled, got.TOTPSecret)
	}
}

func TestTOTPSetupNeedsPassword(t *testing.T) {
	s, user := newTOTPTestServer(t)

	postAs(t, s, user, s.handleTOTPSetup, url.Values{"current_password": {"wrong"}})
	if got, _ := s.App.DB.GetUser(user.ID); got.TOTPSecret != "" {
		t.Fatal("setup started without the right password")
	}

	postAs(t, s, user, s.handleTOTPSetup, url.Values{})
	if got, _ := s.App.DB.GetUser(user.ID); got.TOTPSecret != "" {
		t.Fatal("setup started with no password")
	}

	postAs(t, s, user, s.handleTOTPSetup, url.Values{"current_password": {"correct horse battery"}})
	if got, _ := s.App.DB.GetUser(user.ID); got.TOTPSecret == "" || got.TOTPEnabled {
		t.Fatalf("setup should store a pending, not yet enforced secret: %+v", got)
	}
}

func TestTOTPDisableNeedsPasswordAndIsThrottled(t *testing.T) {
	s, user := newTOTPTestServer(t)
	_ = s.App.DB.SetTOTPSecret(user.ID, "SECRET")
	_ = s.App.DB.SetTOTPEnabled(user.ID, true)

	postAs(t, s, user, s.handleTOTPDisable, url.Values{"current_password": {"wrong"}})
	if got, _ := s.App.DB.GetUser(user.ID); !got.TOTPEnabled {
		t.Fatal("disabled with a wrong password")
	}

	// A stolen session must not be able to guess the password forever.
	for i := 0; i < 6; i++ {
		postAs(t, s, user, s.handleTOTPDisable, url.Values{"current_password": {"guess"}})
	}
	postAs(t, s, user, s.handleTOTPDisable, url.Values{"current_password": {"correct horse battery"}})
	if got, _ := s.App.DB.GetUser(user.ID); !got.TOTPEnabled {
		t.Fatal("password guessing through the settings form was not throttled")
	}
}

func TestTOTPDisableWithPassword(t *testing.T) {
	s, user := newTOTPTestServer(t)
	_ = s.App.DB.SetTOTPSecret(user.ID, "SECRET")
	_ = s.App.DB.SetTOTPEnabled(user.ID, true)

	postAs(t, s, user, s.handleTOTPDisable, url.Values{"current_password": {"correct horse battery"}})
	if got, _ := s.App.DB.GetUser(user.ID); got.TOTPEnabled || got.TOTPSecret != "" {
		t.Fatalf("expected two-factor cleared: %+v", got)
	}
}
