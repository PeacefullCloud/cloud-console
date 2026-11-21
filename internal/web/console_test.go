package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/models"
)

func consoleTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.App.Cfg.TerminalEnabled = true
	s.App.Cfg.TerminalLoginUser = config.DefaultTerminalUser
	s.App.Cfg.TerminalMaxSessions = 2
	s.App.Cfg.TerminalMaxPerUser = 1
	return s
}

func TestConsoleOriginAndHostChecks(t *testing.T) {
	s := consoleTestServer(t)
	s.App.Cfg.TerminalAllowedHosts = []string{"cloud.example.com", "cloud.example.com:8443"}
	s.App.Cfg.TerminalAllowedOrigins = []string{"proxy.example.com"}

	cases := []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{"same origin", "cloud.example.com", "https://cloud.example.com", true},
		{"same origin with port", "cloud.example.com:8443", "https://cloud.example.com:8443", true},
		{"cross site", "cloud.example.com", "https://evil.example", false},
		{"configured origin", "cloud.example.com", "https://proxy.example.com", true},
		{"no origin (non-browser)", "cloud.example.com", "", true},
		{"null origin", "cloud.example.com", "null", false},
		{"malformed origin", "cloud.example.com", "not a url", false},
		{"ftp origin", "cloud.example.com", "ftp://cloud.example.com", false},
		{"host not allowlisted", "evil.example", "https://evil.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/instances/x/console/ws", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := s.consoleRequestAllowed(r); got != tc.want {
				t.Errorf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConsoleQueryIntClamped(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?cols=99999&rows=0", nil)
	if got := queryIntClamped(r, "cols", 80, 1, consoleMaxCols); got != consoleMaxCols {
		t.Errorf("cols = %d, want %d", got, consoleMaxCols)
	}
	if got := queryIntClamped(r, "rows", 24, 1, consoleMaxRows); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}
	if got := queryIntClamped(r, "missing", 80, 1, consoleMaxCols); got != 80 {
		t.Errorf("missing = %d, want default 80", got)
	}
}

func TestConsoleSessionCaps(t *testing.T) {
	s := consoleTestServer(t)
	if !s.consoleAcquire(1) {
		t.Fatal("first session for tenant 1 should be accepted")
	}
	if s.consoleAcquire(1) {
		t.Fatal("per-user cap should reject a second session for tenant 1")
	}
	if !s.consoleAcquire(2) {
		t.Fatal("tenant 2 should still have a free slot")
	}
	if s.consoleAcquire(3) {
		t.Fatal("global cap should reject another tenant")
	}
	s.consoleRelease(1)
	if !s.consoleAcquire(3) {
		t.Fatal("released slot should be reusable")
	}
	s.consoleRelease(2)
	s.consoleRelease(3)
	if s.consoleSessions != 0 {
		t.Errorf("session counter = %d, want 0", s.consoleSessions)
	}
}

func TestConsoleRoutesEnforceTenancyAndRole(t *testing.T) {
	s := consoleTestServer(t)
	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.App.DB.CreateUser("bob", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := s.App.DB.CreateUser("view", "x", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "alice-box", OwnerID: alice.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "viewer-box", OwnerID: viewer.ID}); err != nil {
		t.Fatal(err)
	}

	handlers := map[string]http.HandlerFunc{
		"page":   s.requireInstance(s.handleConsolePage),
		"socket": s.requireInstance(s.handleConsoleSocket),
	}
	for label, handler := range handlers {
		t.Run(label+" other tenant", func(t *testing.T) {
			r := tenantRequest(http.MethodGet, "/instances/alice-box/console", bob)
			r.SetPathValue("name", "alice-box")
			rec := httptest.NewRecorder()
			handler(rec, r)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
		t.Run(label+" viewer", func(t *testing.T) {
			r := tenantRequest(http.MethodGet, "/instances/viewer-box/console", viewer)
			r.SetPathValue("name", "viewer-box")
			rec := httptest.NewRecorder()
			handler(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}

	// An unauthenticated websocket handshake must get a plain 401 rather than
	// a login redirect it cannot follow.
	r := httptest.NewRequest(http.MethodGet, "/instances/alice-box/console/ws", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated socket status = %d, want 401", rec.Code)
	}
}

// The console socket passes through withLogging, whose response wrapper must
// still expose http.Hijacker or gorilla cannot upgrade the connection.
func TestConsoleUpgradeThroughLoggingWrapper(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		conn, err := upgrader.Upgrade(rec, r, nil)
		if err != nil {
			t.Errorf("upgrade through statusRecorder: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ready"))
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, msg, err := conn.ReadMessage()
	if err != nil || string(msg) != "ready" {
		t.Fatalf("read = %q, err = %v", msg, err)
	}
}

func TestConsolePageRenders(t *testing.T) {
	renderer := newTestRenderer(t)
	var buf bytes.Buffer
	data := consoleData{
		baseData:      sampleBase(),
		Instance:      "web-01",
		LoginUser:     config.DefaultTerminalUser,
		Running:       true,
		Cols:          80,
		Rows:          24,
		MaxInputBytes: 262144,
	}
	if err := renderer.ExecuteStandalone(&buf, "templates/console.html", "console_page", data); err != nil {
		t.Fatalf("render console page: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"data-instance=\"web-01\"",
		"data-login-user=\"cloud-user\"",
		"/instances/web-01/console/ws",
		"/static/js/wterm.js",
		"/static/console.js",
		"</html>",
	} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("console page is missing %q", want)
		}
	}
	if bytes.Contains([]byte(out), []byte("{{")) {
		t.Error("console page contains an unrendered template action")
	}
}
