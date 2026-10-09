package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoutesRejectMalformedPathParams(t *testing.T) {
	reached := false
	mux := http.NewServeMux()
	v := validatedMux{mux}
	v.HandleFunc("POST /instances/{name}/backups/{backup}/delete", func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	v.HandleFunc("POST /instances/{name}/snapshots/{snapshot}/delete", func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	v.HandleFunc("POST /domains/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})

	bad := []string{
		"/instances/web/backups/..%2F..%2F..%2Fetc%2Fpasswd/delete",
		"/instances/web/backups/%2e%2e/delete",
		"/instances/web/backups/.hidden/delete",
		"/instances/..%2Fother/backups/x/delete",
		"/instances/we%00b/backups/x/delete",
		"/instances/web/snapshots/a%5Cb/delete",
		"/instances/web/snapshots/a%2Fb/delete",
		"/domains/abc/delete",
		"/domains/1%2F2/delete",
	}
	for _, target := range bad {
		reached = false
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
		if reached || rec.Code != http.StatusNotFound {
			t.Errorf("%s: reached=%v status=%d, want 404 without calling the handler", target, reached, rec.Code)
		}
	}

	good := []string{
		"/instances/web-1/backups/web-1-20240101-1200/delete",
		"/instances/Web1/backups/nightly_v1.2/delete",
		"/instances/web/snapshots/pre-upgrade/delete",
		"/domains/42/delete",
	}
	for _, target := range good {
		reached = false
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
		if !reached {
			t.Errorf("%s: valid request was rejected (status %d)", target, rec.Code)
		}
	}
}
