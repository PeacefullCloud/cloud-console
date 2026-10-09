package alerts

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/models"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestNewValidatesURL(t *testing.T) {
	if n, err := New("", discard); n != nil || err != nil {
		t.Errorf("empty = %v, %v", n, err)
	}
	for _, bad := range []string{"ftp://x", "not a url", "https://", "javascript:alert(1)"} {
		if _, err := New(bad, discard); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := New("https://hooks.example.com/x", discard); err != nil {
		t.Error(err)
	}
}

func TestJobFailedPostsEventWithoutPayload(t *testing.T) {
	got := make(chan event, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e event
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "hunter2") {
			t.Error("the job payload leaked into the alert")
		}
		_ = json.Unmarshal(raw, &e)
		got <- e
	}))
	defer srv.Close()

	n, err := New(srv.URL, discard)
	if err != nil {
		t.Fatal(err)
	}
	n.JobFailed(&models.Job{
		ID: "j1", Kind: "instance.create", Target: "web", CreatedBy: "alice",
		Payload: `{"root_password":"hunter2"}`,
	}, errors.New("boom"))

	select {
	case e := <-got:
		if e.Event != "job.failed" || e.Target != "web" || e.Error != "boom" || e.Text == "" || e.Content != e.Text {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no alert delivered")
	}
}

func TestNilNotifierIsSafe(t *testing.T) {
	var n *Notifier
	n.JobFailed(&models.Job{}, errors.New("x"))
}
