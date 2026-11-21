package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	incusclient "github.com/lxc/incus/v6/client"
	incusapi "github.com/lxc/incus/v6/shared/api"
)

// Wire protocol, shared verbatim with the wterm client in static/console.js.
// The transport encodes every string as a binary frame, so the first byte
// tells the server whether the rest is keystrokes or a control message.
const (
	consoleFrameInput   = byte(0)
	consoleFrameControl = byte(1)

	consoleMaxCols  = 1024
	consoleMaxRows  = 512
	consoleReadSize = 32 * 1024

	consoleWatchdogTick  = 15 * time.Second
	consoleIdleWarnLead  = 60 * time.Second
	consoleWriteDeadline = 10 * time.Second
)

// consoleUpgrader leaves the origin check to consoleRequestAllowed, which can
// use the configuration and the Host header (gorilla's default only compares
// Origin to Host and has no notion of the configured allowlists).
var consoleUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

type consoleData struct {
	baseData
	Instance      string
	LoginUser     string
	Running       bool
	Cols          int
	Rows          int
	MaxInputBytes int64
}

// handleConsolePage renders the standalone terminal window. It is wrapped in
// requireInstance by the router, so a tenant can only open a console for an
// instance they own. A shell can change anything, so read-only roles are
// refused here as well as on the socket.
func (s *Server) handleConsolePage(w http.ResponseWriter, r *http.Request) {
	if !s.App.Cfg.TerminalEnabled {
		http.Error(w, "The web terminal is disabled on this console.", http.StatusForbidden)
		return
	}
	if err := checkWriteAccess(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	name := r.PathValue("name")
	full, err := s.App.Incus.InstanceFull(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	data := consoleData{
		baseData:      s.newBase(w, r, "instances", "Console — "+name),
		Instance:      name,
		LoginUser:     s.App.Cfg.TerminalLoginUser,
		Running:       strings.EqualFold(full.Status, "Running"),
		Cols:          80,
		Rows:          24,
		MaxInputBytes: s.App.Cfg.TerminalMaxInputBytes,
	}
	if err := s.Renderer.RenderStandalone(w, "templates/console.html", "console_page", data); err != nil {
		s.log.Error("render console page", "err", err, "instance", name)
	}
}

// handleConsoleSocket attaches a browser wterm session to an interactive Incus
// exec. Incus allocates the PTY inside the instance, so no SSH port, key or
// password has to be exposed to the browser: the console authenticates the
// tenant, then runs `su - <loginUser>` as root through the local Incus socket.
func (s *Server) handleConsoleSocket(w http.ResponseWriter, r *http.Request) {
	if !s.App.Cfg.TerminalEnabled {
		http.Error(w, "The web terminal is disabled on this console.", http.StatusForbidden)
		return
	}
	if err := checkWriteAccess(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	user := userFrom(r)
	if user == nil {
		http.Error(w, "Login required", http.StatusUnauthorized)
		return
	}
	if !s.consoleRequestAllowed(r) {
		s.log.Warn("console socket refused: bad origin or host",
			"origin", r.Header.Get("Origin"), "host", r.Host, "user", user.Username)
		http.Error(w, "Unexpected Origin or Host header", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")
	cols := queryIntClamped(r, "cols", 80, 1, consoleMaxCols)
	rows := queryIntClamped(r, "rows", 24, 1, consoleMaxRows)

	// Acquire before the upgrade: a rejected request must not open a socket.
	if !s.consoleAcquire(user.ID) {
		http.Error(w, "Too many terminal sessions. Close one and try again.", http.StatusServiceUnavailable)
		return
	}
	released := false
	defer func() {
		if !released {
			s.consoleRelease(user.ID)
		}
	}()

	// A stopped instance cannot run exec. Report that as a normal HTTP error
	// so the page can show it instead of a terminal that silently dies.
	full, err := s.App.Incus.InstanceFull(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !strings.EqualFold(full.Status, "Running") {
		http.Error(w, "The instance is not running. Start it before opening a terminal.", http.StatusConflict)
		return
	}

	conn, err := consoleUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("console websocket upgrade failed", "instance", name, "err", err)
		return
	}
	defer conn.Close()
	// Reject oversized frames in the transport itself, before gorilla allocates
	// memory for them. The per-frame check below stays as a second boundary.
	conn.SetReadLimit(s.App.Cfg.TerminalMaxInputBytes + 1)
	released = true // from here on the socket's own lifecycle releases the slot

	started := time.Now()
	s.log.Info("console session opened", "instance", name, "user", user.Username, "owner", user.ID, "cols", cols, "rows", rows)
	defer func() {
		s.consoleRelease(user.ID)
		s.log.Info("console session closed", "instance", name, "user", user.Username, "duration", time.Since(started).Round(time.Millisecond).String())
	}()

	// Stop the session when either the browser goes away or the server shuts
	// down; a hijacked websocket is not tracked by http.Server.Shutdown.
	ctx, stop := context.WithCancel(r.Context())
	defer stop()
	shutdownCtx := s.consoleCtx
	if shutdownCtx == nil {
		shutdownCtx = context.Background()
	}
	stopOnShutdown := context.AfterFunc(shutdownCtx, stop)
	defer stopOnShutdown()

	s.serveConsole(conn, ctx, name, user.Username, cols, rows)
}

// serveConsole owns everything after the websocket upgrade. It returns only
// when the browser disconnects, the shell exits, a limit is hit, or the server
// shuts down; every goroutine and pipe is cleaned up before it returns.
func (s *Server) serveConsole(conn *websocket.Conn, parent context.Context, name, username string, cols, rows int) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	started := time.Now()

	var (
		writeMu     sync.Mutex
		controlMu   sync.Mutex
		controlConn *websocket.Conn
		inputMu     sync.Mutex
		lastInput   = time.Now()
		closeOnce   sync.Once
	)

	send := func(messageType int, payload []byte) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(consoleWriteDeadline))
		err := conn.WriteMessage(messageType, payload)
		_ = conn.SetWriteDeadline(time.Time{})
		return err == nil
	}
	say := func(msg string) { send(websocket.TextMessage, []byte(msg)) }

	// toGuest is written by the browser reader and read by the Incus mirror;
	// fromGuest is written by the Incus mirror and read by the browser writer.
	toGuestR, toGuestW := io.Pipe()
	fromGuestR, fromGuestW := io.Pipe()
	dataDone := make(chan bool, 1)

	closeAll := func() {
		closeOnce.Do(func() {
			_ = toGuestR.CloseWithError(io.EOF)
			_ = toGuestW.CloseWithError(io.EOF)
			_ = fromGuestR.CloseWithError(io.EOF)
			_ = fromGuestW.CloseWithError(io.EOF)

			controlMu.Lock()
			if controlConn != nil {
				_ = controlConn.Close()
			}
			controlMu.Unlock()

			cancel()
			_ = conn.Close()
		})
	}
	defer closeAll()

	execArgs := &incusclient.InstanceExecArgs{
		Stdin:    toGuestR,
		Stdout:   fromGuestW,
		DataDone: dataDone,
		Control: func(cc *websocket.Conn) {
			controlMu.Lock()
			controlConn = cc
			controlMu.Unlock()
			// Incus sends keepalive frames on the control channel; somebody
			// must consume them or its writer eventually blocks.
			go func() {
				for {
					if _, _, err := cc.ReadMessage(); err != nil {
						return
					}
				}
			}()
		},
	}

	// Incus exec runs as root, so an existing default account needs no
	// password. On instances provisioned before that account existed, drop to
	// a small login prompt instead of letting su print a red "user does not
	// exist" error. The script only uses POSIX shell builtins plus id/su, so
	// it works on glibc, musl and busybox images. The login name is passed as
	// an argument — never interpolated into the script.
	const consoleShell = `if id -u "$1" >/dev/null 2>&1; then exec su - "$1"; fi
attempt=0
while [ "$attempt" -lt 3 ]; do
  printf '\r\n%s login: ' "$(hostname 2>/dev/null || echo console)"
  read -r console_user
  case "$console_user" in
    ''|*[!A-Za-z0-9_.-]*) attempt=$((attempt + 1)); continue ;;
  esac
  if id -u "$console_user" >/dev/null 2>&1; then exec su - "$console_user"; fi
  printf 'Login incorrect\r\n'
  attempt=$((attempt + 1))
done
exec /bin/sh -l`
	op, err := s.App.Incus.SDK().ExecInstance(name, incusapi.InstanceExecPost{
		Command:     []string{"sh", "-c", consoleShell, "console", s.App.Cfg.TerminalLoginUser},
		WaitForWS:   true,
		Interactive: true,
		Width:       cols,
		Height:      rows,
		Environment: map[string]string{"TERM": "xterm-256color"},
	}, execArgs)
	if err != nil {
		s.log.Warn("console exec failed", "instance", name, "err", err)
		say("\r\n\x1b[31mCould not start a shell in this instance: " + err.Error() + "\x1b[0m\r\n")
		return
	}
	defer func() {
		closeAll() // unblock the Incus mirrors before waiting for them
		_ = op.Cancel()
		select {
		case <-dataDone:
		case <-time.After(2 * time.Second):
		}
	}()

	say(fmt.Sprintf("\r\n\x1b[32mConnected to %s.\x1b[0m\r\n", name))

	// done carries the first terminal event; its value is only for the log.
	done := make(chan string, 4)

	// Browser -> guest.
	go func() {
		defer func() { done <- "client-closed" }()
		defer toGuestW.Close()
		for {
			if ctx.Err() != nil {
				return
			}
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if messageType != websocket.BinaryMessage || len(data) == 0 {
				continue
			}
			inputMu.Lock()
			lastInput = time.Now()
			inputMu.Unlock()

			if int64(len(data)) > s.App.Cfg.TerminalMaxInputBytes {
				say("\r\n\x1b[31mInput frame too large — session closed.\x1b[0m\r\n")
				return
			}

			switch data[0] {
			case consoleFrameInput:
				if len(data) > 1 {
					if _, err := toGuestW.Write(data[1:]); err != nil {
						return
					}
				}
			case consoleFrameControl:
				var control struct {
					Type string `json:"t"`
					Cols int    `json:"c"`
					Rows int    `json:"r"`
				}
				if json.Unmarshal(data[1:], &control) != nil || control.Type != "r" ||
					control.Cols < 1 || control.Cols > consoleMaxCols ||
					control.Rows < 1 || control.Rows > consoleMaxRows {
					continue
				}
				controlMu.Lock()
				cc := controlConn
				if cc != nil {
					_ = cc.WriteJSON(incusapi.InstanceExecControl{
						Command: "window-resize",
						Args: map[string]string{
							"width":  strconv.Itoa(control.Cols),
							"height": strconv.Itoa(control.Rows),
						},
					})
				}
				controlMu.Unlock()
			default:
				// Only the two defined frame types may reach the shell. An
				// unknown tag is a malformed or hostile client, not a reason
				// to forward arbitrary bytes into the guest.
				continue
			}
		}
	}()

	// Guest -> browser.
	go func() {
		defer func() { done <- "shell-exit" }()
		defer fromGuestR.Close()
		buf := make([]byte, consoleReadSize)
		for {
			n, err := fromGuestR.Read(buf)
			if n > 0 {
				if !send(websocket.BinaryMessage, buf[:n]) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Session watchdog: absolute lifetime, idle timeout with a warning, and
	// server-shutdown cancellation.
	go func() {
		warned := false
		ticker := time.NewTicker(consoleWatchdogTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- "shutdown"
				return
			case <-ticker.C:
				now := time.Now()
				if limit := s.App.Cfg.TerminalMaxSession; limit > 0 && now.Sub(started) >= limit {
					say("\r\n\x1b[33mMaximum session time reached — disconnecting.\x1b[0m\r\n")
					done <- "max-lifetime"
					return
				}
				idle := s.App.Cfg.TerminalIdleTimeout
				if idle <= 0 {
					continue
				}
				inputMu.Lock()
				quiet := now.Sub(lastInput)
				inputMu.Unlock()
				if quiet >= idle {
					say("\r\n\x1b[33mIdle timeout — disconnecting.\x1b[0m\r\n")
					done <- "idle-timeout"
					return
				}
				lead := consoleIdleWarnLead
				if idle <= 2*lead {
					lead = 0
				}
				if lead > 0 && !warned && quiet >= idle-lead {
					warned = true
					say("\r\n\x1b[33mIdle timeout in about a minute — press a key to stay connected.\x1b[0m\r\n")
				} else if warned && quiet < idle-lead {
					warned = false
				}
			}
		}
	}()

	reason := <-done
	s.log.Info("console session ending", "instance", name, "user", username, "reason", reason)
}

// consoleRequestAllowed implements the two checks a plain cookie check cannot:
// a browser's Origin must match the public host, and (when configured) the
// Host header must be a name the operator trusts. Together they stop cross-site
// websocket hijacking and DNS rebinding from reaching the terminal socket.
func (s *Server) consoleRequestAllowed(r *http.Request) bool {
	if hosts := s.App.Cfg.TerminalAllowedHosts; len(hosts) > 0 && !hostAllowed(r.Host, hosts) {
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients do not send Origin; the session cookie still
		// has to be present before this handler is reached.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, allowed := range s.App.Cfg.TerminalAllowedOrigins {
		if strings.EqualFold(u.Host, allowed) || strings.EqualFold(origin, allowed) {
			return true
		}
	}
	return false
}

func hostAllowed(host string, allowed []string) bool {
	for _, name := range allowed {
		if strings.EqualFold(host, name) {
			return true
		}
	}
	return false
}

func queryIntClamped(r *http.Request, key string, def, min, max int) int {
	value := atoiDefault(r.URL.Query().Get(key), def)
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// consoleAcquire enforces both the host-wide and the per-tenant session caps.
// The per-user cap matters in a multi-tenant console: one tenant cannot fill
// every slot and lock the others out of their own instances.
func (s *Server) consoleAcquire(userID int64) bool {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	if s.consoleByUser == nil {
		s.consoleByUser = map[int64]int{}
	}
	if max := s.App.Cfg.TerminalMaxSessions; max > 0 && s.consoleSessions >= max {
		return false
	}
	if s.consoleByUser[userID] >= s.App.Cfg.TerminalMaxPerUser {
		return false
	}
	s.consoleSessions++
	s.consoleByUser[userID]++
	return true
}

func (s *Server) consoleRelease(userID int64) {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	if s.consoleSessions > 0 {
		s.consoleSessions--
	}
	if s.consoleByUser[userID] > 0 {
		s.consoleByUser[userID]--
	}
}
