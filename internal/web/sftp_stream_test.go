package web

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/incus"
)

func TestLiveSFTPLargeStreamingDownload(t *testing.T) {
	if os.Getenv("INCUS_LIVE_TEST") != "1" {
		t.Skip("live")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	client, err := incus.Connect("/var/lib/incus/unix.socket", "", log)
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(t.TempDir(), "console.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := &config.Config{TerminalEnabled: true, TerminalLoginUser: "cloud-user", TerminalMaxSessions: 20, TerminalMaxPerUser: 4}
	app := &core.App{Cfg: cfg, DB: db, Incus: client, Auth: auth.New(db, cfg, log), Log: log}
	s := &Server{Deps: Deps{App: app}, log: log, sftpSessions: map[string]*sftpSession{}}
	s.consoleCtx = context.Background()
	user, err := db.CreateUser("probe", "x", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token := "probe-token"
	if err := db.CreateSession(auth.HashToken(token), user.ID, time.Now().Add(time.Hour), "127.0.0.1", "probe"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("name", "testing101")
		r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
		r = r.WithContext(context.WithValue(r.Context(), tokenKey{}, token))
		s.handleSFTPSocket(w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(120 * time.Second))

	name := fmt.Sprintf("probe-%d.bin", time.Now().UnixNano())

	payload := make([]byte, 1024*1024+123)
	for i := range payload {
		payload[i] = byte(i*i + 7)
	}
	want := sha256.Sum256(payload)

	// Upload via websocket chunk protocol.
	upID := uint64(700001)
	call := func(id uint64, op string, extra map[string]any) map[string]any {
		msg := map[string]any{"op": op, "id": id}
		for k, v := range extra {
			msg[k] = v
		}
		if err := ws.WriteJSON(msg); err != nil {
			t.Fatal(err)
		}
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				t.Fatal(err)
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
					t.Fatalf("%s: %v", op, resp["error"])
				}
				return resp
			}
		}
	}
	call(upID, "upload_begin", map[string]any{"path": "/home/cloud-user/" + name, "size": len(payload)})
	for off := 0; off < len(payload); {
		n := 256 * 1024
		if off+n > len(payload) {
			n = len(payload) - off
		}
		frame := make([]byte, 16+n)
		binary.BigEndian.PutUint64(frame[0:8], upID)
		binary.BigEndian.PutUint64(frame[8:16], uint64(off))
		copy(frame[16:], payload[off:off+n])
		if err := ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatal(err)
		}
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if mt == websocket.TextMessage {
				var ack map[string]any
				if json.Unmarshal(data, &ack) == nil && ack["op"] == "write" && ack["id"] == float64(upID) {
					break
				}
			}
		}
		off += n
	}
	call(upID, "upload_end", nil)

	// Download with the streaming protocol the browser uses: one begin, then
	// tagged chunks placed by absolute offset and a single done message.
	downID := uint64(700002)
	begin := call(downID, "download_begin", map[string]any{"path": "/home/cloud-user/" + name})
	size := int64(begin["size"].(float64))
	got := make([]byte, size)
	var done bool
	for !done {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if mt == websocket.TextMessage {
			var resp map[string]any
			if json.Unmarshal(data, &resp) == nil && resp["id"] == float64(downID) &&
				(resp["op"] == "download_done" || resp["op"] == "download_error") {
				if resp["ok"] != true {
					t.Fatalf("download error: %v", resp["error"])
				}
				done = true
			}
		}
		if mt == websocket.BinaryMessage && len(data) >= 16 {
			id := binary.BigEndian.Uint64(data[0:8])
			off := binary.BigEndian.Uint64(data[8:16])
			if id == downID && int64(off)+int64(len(data)-16) <= size {
				copy(got[off:], data[16:])
			}
		}
	}
	gotHash := sha256.Sum256(got)
	if gotHash != want {
		t.Fatalf("hash mismatch: got %x want %x, len %d/%d", gotHash, want, len(got), len(payload))
	}
	t.Logf("download %d bytes ok", len(got))
}
