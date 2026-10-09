package web

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/activity"
	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/models"
)

func TestMetricsEndpointIsOffWithoutAToken(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestMetricsEndpointRequiresTheToken(t *testing.T) {
	s := newTestServer(t)
	s.App.Cfg.MetricsToken = "0123456789abcdef-secret"

	for name, header := range map[string]string{
		"none":         "",
		"wrong":        "Bearer nope",
		"prefix only":  "Bearer 0123456789abcdef",
		"no scheme":    "0123456789abcdef-secrex",
		"empty bearer": "Bearer ",
	} {
		req := httptest.NewRequest("GET", "/metrics", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		s.handleMetrics(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "console_up") {
			t.Errorf("%s: metrics leaked", name)
		}
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef-secret")
	if !s.metricsAuthorized(req) {
		t.Error("the right token was refused")
	}
}

func TestLabelEscaping(t *testing.T) {
	if got := label("a\"b\\c\nd"); got != `"a\"b\\c\nd"` {
		t.Errorf("label = %s", got)
	}
}

func TestCSVSafeNeutralisesFormulas(t *testing.T) {
	for in, want := range map[string]string{
		"=HYPERLINK(\"http://evil\")": "'=HYPERLINK(\"http://evil\")",
		"+1+1":                        "'+1+1",
		"-2":                          "'-2",
		"@SUM(A1)":                    "'@SUM(A1)",
		"\tcmd":                       "'\tcmd",
		"normal":                      "normal",
		"":                            "",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func exportRequest(user *models.User) *http.Request {
	req := httptest.NewRequest("GET", "/activity/export.csv", nil)
	return req.WithContext(context.WithValue(req.Context(), userKey{}, user))
}

func TestActivityExportIsAdminOnlyAndSafe(t *testing.T) {
	s := newTestServer(t)
	s.App.Activity = activity.New(s.App.DB, s.log)
	s.App.Activity.Record("alice", "Delete instance", "=cmd|' /C calc'!A0", "", nil)

	for _, role := range []string{auth.RoleViewer, auth.RoleOperator} {
		rec := httptest.NewRecorder()
		s.handleActivityExport(rec, exportRequest(&models.User{Username: "x", Role: role}))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", role, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.handleActivityExport(rec, exportRequest(nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("anonymous: status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handleActivityExport(rec, exportRequest(&models.User{Username: "root", Role: auth.RoleAdmin}))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content type = %q", ct)
	}
	rows, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 || rows[0][0] != "time_utc" {
		t.Fatalf("rows = %v", rows)
	}
	found := false
	for _, r := range rows[1:] {
		if r[2] == "Delete instance" {
			found = true
			if !strings.HasPrefix(r[3], "'=") {
				t.Errorf("formula not neutralised: %q", r[3])
			}
			if _, err := time.Parse(time.RFC3339, r[0]); err != nil {
				t.Errorf("bad timestamp %q", r[0])
			}
		}
	}
	if !found {
		t.Error("the recorded entry is missing from the export")
	}
}
