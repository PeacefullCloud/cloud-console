package web

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The SSE handler must outlive http.Server.WriteTimeout, including through the
// logging wrapper that sits in front of it in production.
func TestStreamOutlivesWriteTimeout(t *testing.T) {
	s := newTestServer(t)

	h := s.withLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
			t.Errorf("SetWriteDeadline through the wrapper: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("data: late\n\n"))
		w.(http.Flusher).Flush()
	}))

	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = 150 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: late\n" {
		t.Fatalf("stream cut by the write timeout: %q, %v", line, err)
	}
}
