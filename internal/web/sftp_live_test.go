package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// Live check of the SFTP-over-Incus-exec transport used by the console file
// panel. Guarded by INCUS_LIVE_TEST=1 so the normal suite never touches Incus.
// Run: sg incus-admin -c "INCUS_LIVE_TEST=1 go test ./internal/web -run TestLiveSFTP -v -count=1"
func TestLiveSFTP(t *testing.T) {
	if os.Getenv("INCUS_LIVE_TEST") != "1" {
		t.Skip("live incus test; set INCUS_LIVE_TEST=1")
	}
	name := os.Getenv("INCUS_LIVE_INSTANCE")
	if name == "" {
		name = "testing101"
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	client, err := incus.Connect("/var/lib/incus/unix.socket", "", log)
	if err != nil {
		t.Fatalf("connect incus: %v", err)
	}
	db, err := database.Open(filepath.Join(t.TempDir(), "console.db"), log)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cfg := &config.Config{TerminalEnabled: true, TerminalLoginUser: "cloud-user"}
	app := &core.App{Cfg: cfg, DB: db, Incus: client, Log: log}
	s := &Server{Deps: Deps{App: app}, log: log, sftpSessions: map[string]*sftpSession{}}
	s.consoleCtx = context.Background()

	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), userKey{}, &models.User{ID: 1, Username: "admin", Role: "admin"}))
	r = r.WithContext(context.WithValue(r.Context(), tokenKey{}, "live-test-token"))

	sess, err := s.openSFTP(context.Background(), r, name)
	if err != nil {
		t.Fatalf("openSFTP: %v", err)
	}
	defer sess.shutdown()
	t.Logf("sftp root = %s", sess.root)

	entries, err := sess.client.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	t.Logf("home entries = %d", len(entries))

	// Round-trip a small file to prove reads and writes both work over the pipe.
	remote := sess.root + "/.console-sftp-live-test"
	f, err := sess.client.OpenFile(remote, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	payload := []byte("sftp-live-ok\n")
	if _, err := f.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	defer sess.client.Remove(remote)

	rf, err := sess.client.Open(remote)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rf)
	_ = rf.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round trip = %q, want %q", got, payload)
	}
	t.Log("sftp round trip ok")
}

// Full websocket-level check of the file panel protocol on a live instance.
// Run: sg incus-admin -c "INCUS_LIVE_TEST=1 go test ./internal/web -run TestLiveSFTPPanel -v -count=1"
func TestLiveSFTPPanel(t *testing.T) {
	if os.Getenv("INCUS_LIVE_TEST") != "1" {
		t.Skip("live incus test; set INCUS_LIVE_TEST=1")
	}
	name := os.Getenv("INCUS_LIVE_INSTANCE")
	if name == "" {
		name = "testing101"
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	client, err := incus.Connect("/var/lib/incus/unix.socket", "", log)
	if err != nil {
		t.Fatalf("connect incus: %v", err)
	}
	db, err := database.Open(filepath.Join(t.TempDir(), "console.db"), log)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cfg := &config.Config{
		TerminalEnabled: true, TerminalLoginUser: "cloud-user",
		TerminalMaxSessions: 20, TerminalMaxPerUser: 4,
	}
	app := &core.App{Cfg: cfg, DB: db, Incus: client, Auth: auth.New(db, cfg, log), Log: log}
	s := &Server{Deps: Deps{App: app}, log: log, sftpSessions: map[string]*sftpSession{}}
	s.consoleCtx = context.Background()

	user, err := db.CreateUser("panel-admin", "x", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := "panel-live-token"
	if err := db.CreateSession(auth.HashToken(token), user.ID, time.Now().Add(time.Hour), "127.0.0.1", "test"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("name", name)
		r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
		r = r.WithContext(context.WithValue(r.Context(), tokenKey{}, token))
		s.handleSFTPSocket(w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))

	var msgID atomic.Uint64
	call := func(op string, extra map[string]any) map[string]any {
		t.Helper()
		id := msgID.Add(1)
		if v, ok := extra["id"]; ok {
			id = v.(uint64)
		}
		msg := map[string]any{"op": op, "id": id}
		for k, v := range extra {
			msg[k] = v
		}
		if err := ws.WriteJSON(msg); err != nil {
			t.Fatalf("send %s: %v", op, err)
		}
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				t.Fatalf("read %s: %v", op, err)
			}
			if mt != websocket.TextMessage {
				continue
			}
			var resp map[string]any
			if json.Unmarshal(data, &resp) != nil {
				continue
			}
			if resp["id"] == float64(id) {
				if resp["ok"] != true {
					t.Fatalf("%s failed: %v", op, resp["error"])
				}
				return resp
			}
		}
	}

	ready := call("ping", nil)
	if ready["op"] != "pong" {
		t.Fatalf("ping = %v", ready)
	}

	base := fmt.Sprintf(".console-panel-live-%d", time.Now().UnixNano())
	call("mkdir", map[string]any{"dest": base})
	t.Log("mkdir ok")

	// Upload a file through the binary chunk protocol.
	uploadID := uint64(9001)
	content := []byte("panel round trip\n")
	call("upload_begin", map[string]any{"id": uploadID, "path": base + "/note.txt", "size": len(content)})
	frame := make([]byte, 16+len(content))
	binary.BigEndian.PutUint64(frame[0:8], uploadID)
	binary.BigEndian.PutUint64(frame[8:16], 0)
	copy(frame[16:], content)
	if err := ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	if err := ws.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read ack: %v", err)
		}
		if mt == websocket.TextMessage {
			var ack map[string]any
			if json.Unmarshal(data, &ack) == nil && ack["op"] == "write" && ack["id"] == float64(uploadID) {
				break
			}
		}
	}
	call("upload_end", map[string]any{"id": uploadID})
	t.Log("upload ok")

	// Download it back with the streaming protocol the browser uses: one
	// begin request, then tagged chunks and a single download_done message.
	downloadID := uint64(9002)
	begin := call("download_begin", map[string]any{"id": downloadID, "path": base + "/note.txt"})
	if begin["size"] != float64(len(content)) {
		t.Fatalf("download size = %v", begin["size"])
	}
	got := make([]byte, len(content))
	var done bool
	for !done {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read chunk: %v", err)
		}
		if mt == websocket.TextMessage {
			var resp map[string]any
			if json.Unmarshal(data, &resp) == nil && resp["id"] == float64(downloadID) &&
				(resp["op"] == "download_done" || resp["op"] == "download_error") {
				if resp["ok"] != true {
					t.Fatalf("download error: %v", resp["error"])
				}
				done = true
			}
		}
		if mt == websocket.BinaryMessage && len(data) >= 16 {
			id := binary.BigEndian.Uint64(data[0:8])
			offset := binary.BigEndian.Uint64(data[8:16])
			if id == downloadID && int(offset)+len(data)-16 <= len(got) {
				copy(got[offset:], data[16:])
			}
		}
	}
	if string(got) != string(content) {
		t.Fatalf("download = %q, want %q", got, content)
	}
	t.Log("download ok")

	list := call("list", map[string]any{"path": base})
	entries, _ := list["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("list = %d entries, want 1", len(entries))
	}

	call("mkdir", map[string]any{"path": base, "dest": "dest"})
	call("copy", map[string]any{"paths": []string{base + "/note.txt"}, "dest": base + "/dest"})
	call("rename", map[string]any{"path": base + "/dest/note.txt", "dest": base + "/dest/renamed.txt"})
	call("remove", map[string]any{"paths": []string{base}})
	t.Log("copy, rename, delete ok")

	// Revoking the console session must close an already-open file panel.
	s.revokeSFTPUser(user.ID, "")
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("socket stayed open after session revocation")
	}
	t.Log("session revocation closed the socket")
}
