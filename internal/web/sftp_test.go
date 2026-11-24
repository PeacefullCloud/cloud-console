package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/models"
)

// The file panel's websocket goes through the same ownership guard as the
// terminal: another tenant gets 404 (no name probing) and a viewer gets 403.
func TestSFTPRoutesEnforceTenancyAndRole(t *testing.T) {
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
	for _, meta := range []models.InstanceMeta{
		{Name: "alice-box", OwnerID: alice.ID},
		{Name: "viewer-box", OwnerID: viewer.ID},
	} {
		if err := s.App.DB.SaveInstanceMeta(&meta); err != nil {
			t.Fatal(err)
		}
	}

	handler := s.requireInstance(s.handleSFTPSocket)
	cases := []struct {
		name string
		user *models.User
		inst string
		want int
	}{
		{"other tenant", bob, "alice-box", http.StatusNotFound},
		{"viewer", viewer, "viewer-box", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tenantRequest(http.MethodGet, "/instances/"+tc.inst+"/console/sftp/ws", tc.user)
			r.SetPathValue("name", tc.inst)
			r.Header.Set("Upgrade", "websocket")
			r.Header.Set("Connection", "Upgrade")
			rec := httptest.NewRecorder()
			handler(rec, r)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestSFTPPathConfinement(t *testing.T) {
	root := "/home/cloud-user"
	cases := []struct {
		path string
		want bool
	}{
		{root, true},
		{root + "/notes/a.txt", true},
		{"/home/cloud-user-evil/x", false},
		{"/etc/passwd", false},
		{"/", false},
		{root + "/../other", false},
	}
	for _, tc := range cases {
		if got := withinRoot(root, tc.path); got != tc.want {
			t.Errorf("withinRoot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSFTPRejectsMaliciousNamesAndPaths(t *testing.T) {
	sess := &sftpSession{root: "/home/cloud-user"}

	badNames := []string{
		"", ".", "..", "a/b", `a\b`, "a\x00b", "a\nb", "a\x1bb", strings.Repeat("x", 256),
	}
	for _, name := range badNames {
		if _, err := sess.resolveChild("/home/cloud-user", name); err == nil {
			t.Errorf("resolveChild accepted %q", name)
		}
	}
	if got, err := sess.resolveChild("/home/cloud-user", "  notes  "); err != nil || got != "/home/cloud-user/notes" {
		t.Errorf("resolveChild trimmed name = %q, err = %v", got, err)
	}

	for _, p := range []string{"/home/cloud-user/a\x00b", "/home/cloud-user/a\nb", "/home/cloud-user/a\x1bb", strings.Repeat("x", 5000)} {
		if _, err := sess.resolve(p); err == nil {
			t.Errorf("resolve accepted %q", p)
		}
	}
}

func TestSFTPRevocationIsPerUser(t *testing.T) {
	s := newTestServer(t)
	s.sftpSessions = map[string]*sftpSession{}
	var aliceClosed, bobClosed atomic.Int32

	s.sftpSessions["alice"] = &sftpSession{userID: 1, wsCancel: func() { aliceClosed.Add(1) }}
	s.sftpSessions["bob"] = &sftpSession{userID: 2, wsCancel: func() { bobClosed.Add(1) }}

	s.revokeSFTPUser(1, "")
	if aliceClosed.Load() != 1 || bobClosed.Load() != 0 {
		t.Fatalf("revoked alice=%d bob=%d, want 1 and 0", aliceClosed.Load(), bobClosed.Load())
	}
	if _, ok := s.sftpSessions["alice"]; ok {
		t.Fatal("revoked session is still registered")
	}
	if _, ok := s.sftpSessions["bob"]; !ok {
		t.Fatal("other tenant's session was revoked")
	}
}

func TestSFTPMultiInstanceIsolationPerToken(t *testing.T) {
	s := newTestServer(t)
	s.sftpSessions = map[string]*sftpSession{}
	var boxAClosed, boxBClosed atomic.Int32

	keyA := sftpSessionKey("hash1", "box-a")
	keyB := sftpSessionKey("hash1", "box-b")
	s.sftpSessions[keyA] = &sftpSession{userID: 1, tokenHash: "hash1", instance: "box-a", wsCancel: func() { boxAClosed.Add(1) }}
	s.sftpSessions[keyB] = &sftpSession{userID: 1, tokenHash: "hash1", instance: "box-b", wsCancel: func() { boxBClosed.Add(1) }}

	// Closing box-b's tab must not tear down box-a's SFTP session.
	s.closeSFTP("hash1", "box-b")
	if boxAClosed.Load() != 0 || boxBClosed.Load() != 1 {
		t.Fatalf("closed box-a=%d box-b=%d, want 0 and 1", boxAClosed.Load(), boxBClosed.Load())
	}
	if _, ok := s.sftpSessions[keyA]; !ok {
		t.Fatal("box-a session should remain active")
	}
}

