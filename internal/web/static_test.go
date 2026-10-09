package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestStaticDirectoriesAreNotListed(t *testing.T) {
	files := fstest.MapFS{
		"app.js":         {Data: []byte("// js")},
		"img/logo.svg":   {Data: []byte("<svg/>")},
		"img/secret.txt": {Data: []byte("x")},
	}
	h := noDirectoryListing(http.FileServerFS(files))

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/app.js"); rec.Code != http.StatusOK {
		t.Errorf("/app.js = %d, want 200", rec.Code)
	}
	if rec := get("/img/logo.svg"); rec.Code != http.StatusOK {
		t.Errorf("/img/logo.svg = %d, want 200", rec.Code)
	}
	for _, path := range []string{"/", "/img/", "/img"} {
		rec := get(path)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "secret.txt") {
			t.Errorf("%s exposed a directory listing (status %d)", path, rec.Code)
		}
	}
}
