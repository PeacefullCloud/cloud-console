package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	incusclient "github.com/lxc/incus/v6/client"
	incusapi "github.com/lxc/incus/v6/shared/api"
	"github.com/pkg/sftp"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/database"
)

// The file panel speaks SFTP to the instance without opening an SSH port or
// storing a private key. sftp-server runs inside the guest through the same
// Incus exec path as the terminal, as the terminal login user, with its working
// directory set to that user's home. Every browser connection gets its own
// sftp.Client and is bound to the console session token that created it.

const (
	sftpOpenTimeout  = 20 * time.Second
	sftpIdleTimeout  = 15 * time.Minute
	sftpSessionTick  = 20 * time.Second
	sftpMaxChunk     = 512 * 1024
	sftpReadLimit    = 4 << 20
	sftpWriteTimeout = 30 * time.Second

	// Bounds on client-supplied data so a hostile browser cannot grow the
	// per-session transfer maps or walk an unbounded list of paths.
	sftpMaxPathLen    = 4096
	sftpMaxNameLen    = 255
	sftpMaxTransfers  = 32
	sftpMaxBatchItems = 100
)

// sftpServerShell locates sftp-server and runs it as the terminal user from
// their home directory. The login name is passed as an argument, never
// interpolated into the script. Instances without that account (created
// before the feature) simply cannot open the panel.
const sftpServerShell = `find_sftp() {
  for p in /usr/lib/openssh/sftp-server /usr/libexec/openssh/sftp-server \
           /usr/lib/ssh/sftp-server /usr/lib64/openssh/sftp-server \
           /usr/sbin/sftp-server /usr/lib/sftp-server; do
    if [ -x "$p" ]; then printf '%s' "$p"; return 0; fi
  done
  return 1
}
srv=$(find_sftp) || { echo "sftp-server is not installed" >&2; exit 127; }
u="$1"
case "$u" in ''|*[!A-Za-z0-9_.-]*) echo "invalid login user" >&2; exit 2;; esac
home="/home/$u"
[ -d "$home" ] || { echo "home directory $home is missing" >&2; exit 3; }
if id -u "$u" >/dev/null 2>&1; then
  exec su -s /bin/sh -c "cd '$home' && exec $srv" "$u"
fi
echo "login user $u does not exist" >&2
exit 4`

type sftpWriteJob struct {
	offset int64
	data   []byte
}

type sftpUpload struct {
	file   *sftp.File
	ch     chan sftpWriteJob
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

func startSFTPUpload(conn *sftpConn, id uint64, f *sftp.File) *sftpUpload {
	u := &sftpUpload{
		file: f,
		ch:   make(chan sftpWriteJob, 16),
		done: make(chan struct{}),
	}
	go func() {
		defer close(u.done)
		for job := range u.ch {
			select {
			case <-conn.done:
				return
			default:
			}
			_, err := u.file.WriteAt(job.data, job.offset)
			if err != nil {
				_ = conn.send(sftpMessage{ID: id, Op: "write", OK: false, Offset: job.offset, Error: friendlySFTPError(err)})
				continue
			}
			_ = conn.send(sftpMessage{ID: id, Op: "write", OK: true, Offset: job.offset})
		}
	}()
	return u
}

func (u *sftpUpload) enqueue(job sftpWriteJob) (ok bool, full bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return false, false
	}
	select {
	case u.ch <- job:
		return true, false
	default:
		return false, true
	}
}

func (u *sftpUpload) finish() error {
	u.mu.Lock()
	if !u.closed {
		u.closed = true
		close(u.ch)
	}
	u.mu.Unlock()
	<-u.done
	return u.file.Close()
}

func (u *sftpUpload) abort() {
	u.mu.Lock()
	if !u.closed {
		u.closed = true
		close(u.ch)
	}
	u.mu.Unlock()
	_ = u.file.Close()
}

// sftpSession is one browser's SFTP connection. The client is safe for
// concurrent use; transfers guards the active upload/download maps.
type sftpSession struct {
	client     *sftp.Client
	root       string
	userID     int64
	username   string
	token      string
	tokenHash  string
	instance   string
	cancel     context.CancelFunc // cancels the Incus exec
	wsCancel   context.CancelFunc // closes the browser socket on revocation
	closePipes func()             // unblocks the SFTP reader on teardown
	closer     sync.Once

	mu       sync.Mutex
	lastUsed time.Time

	// transfers guards the file maps: async upload workers and streaming
	// download goroutines can run concurrently with the socket reader.
	transfers sync.Mutex
	uploads   map[uint64]*sftpUpload
	downloads map[uint64]*sftp.File
}

func (sess *sftpSession) touch() {
	sess.mu.Lock()
	sess.lastUsed = time.Now()
	sess.mu.Unlock()
}

func (sess *sftpSession) idle() time.Duration {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return time.Since(sess.lastUsed)
}

// shutdown is idempotent so an exec ending, a logout revocation and a socket
// close can all call it from different goroutines.
func (sess *sftpSession) shutdown() {
	sess.closer.Do(func() {
		if sess.closePipes != nil {
			sess.closePipes()
		}
		if sess.wsCancel != nil {
			sess.wsCancel()
		}
		if sess.cancel != nil {
			sess.cancel()
		}
		sess.transfers.Lock()
		for _, u := range sess.uploads {
			u.abort()
		}
		for _, f := range sess.downloads {
			_ = f.Close()
		}
		sess.transfers.Unlock()
		if sess.client != nil {
			_ = sess.client.Close()
		}
	})
}

var errSFTPPath = errors.New("path is outside this session's home directory")

// resolve turns a browser-supplied path into a path confined to the session's
// home. Symlinks are resolved with the server's RealPath and checked again, so
// a link inside the home cannot escape it.
func (sess *sftpSession) resolve(p string) (string, error) {
	if len(p) > sftpMaxPathLen {
		return "", errors.New("path is too long")
	}
	for _, r := range p {
		// Control characters in a path are never legitimate here and would
		// otherwise become terminal escape sequences if copied back.
		if r == 0 || r < 0x20 || r == 0x7f {
			return "", errors.New("invalid characters in path")
		}
	}
	switch p {
	case "", "~", "/~":
		return sess.root, nil
	}
	if strings.HasPrefix(p, "~/") {
		p = sess.root + strings.TrimPrefix(p, "~")
	} else if !strings.HasPrefix(p, "/") {
		p = path.Join(sess.root, p)
	}
	p = path.Clean(p)
	if !withinRoot(sess.root, p) {
		return "", errSFTPPath
	}
	if sess.client != nil {
		if real, err := sess.client.RealPath(p); err == nil {
			real = path.Clean(real)
			if !withinRoot(sess.root, real) {
				return "", errSFTPPath
			}
			return real, nil
		}
	}
	// The target may not exist yet (an upload/mkdir destination); the lexical
	// containment check above is authoritative in that case.
	return p, nil
}

func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// openSFTP starts sftp-server in the instance and registers the client against
// the caller's session token. An existing connection for the token is reused.
func (s *Server) openSFTP(ctx context.Context, r *http.Request, name string) (*sftpSession, error) {
	token := tokenFrom(r)
	if token == "" {
		return nil, errors.New("no console session")
	}
	hash := auth.HashToken(token)
	user := userFrom(r)
	if user == nil {
		return nil, errors.New("no console session")
	}

	s.sftpMu.Lock()
	if existing, ok := s.sftpSessions[hash]; ok {
		s.sftpMu.Unlock()
		existing.touch()
		return existing, nil
	}
	s.sftpMu.Unlock()

	toGuestR, toGuestW := io.Pipe()
	fromGuestR, fromGuestW := io.Pipe()
	dataDone := make(chan bool)
	execCtx, cancelExec := context.WithCancel(s.consoleCtx)
	cleanupPipes := func() {
		_ = toGuestR.Close()
		_ = toGuestW.Close()
		_ = fromGuestR.Close()
		_ = fromGuestW.Close()
	}

	execArgs := &incusclient.InstanceExecArgs{
		Stdin:    toGuestR,
		Stdout:   fromGuestW,
		Stderr:   io.Discard, // never let server logs corrupt the SFTP stream
		DataDone: dataDone,
		Control: func(cc *websocket.Conn) {
			go func() {
				for {
					if _, _, err := cc.ReadMessage(); err != nil {
						return
					}
				}
			}()
		},
	}
	op, err := s.App.Incus.SDK().ExecInstance(name, incusapi.InstanceExecPost{
		Command:     []string{"sh", "-c", sftpServerShell, "sftp-server", s.App.Cfg.TerminalLoginUser},
		WaitForWS:   true,
		Interactive: false,
	}, execArgs)
	if err != nil {
		cancelExec()
		cleanupPipes()
		return nil, fmt.Errorf("start sftp-server: %w", err)
	}
	go func() { <-dataDone }()

	type handshake struct {
		client *sftp.Client
		wd     string
		err    error
	}
	result := make(chan handshake, 1)
	go func() {
		client, err := sftp.NewClientPipe(
			fromGuestR, toGuestW,
			sftp.MaxPacketUnchecked(128*1024),
			sftp.UseConcurrentWrites(true),
		)
		if err != nil {
			result <- handshake{err: err}
			return
		}
		wd, err := client.Getwd()
		result <- handshake{client: client, wd: wd, err: err}
	}()

	timer := time.NewTimer(sftpOpenTimeout)
	var hs handshake
	select {
	case hs = <-result:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	case <-timer.C:
		cancelExec()
		cleanupPipes()
		go func() { _ = op.Wait() }()
		return nil, errors.New("sftp-server did not respond in time")
	case <-ctx.Done():
		cancelExec()
		cleanupPipes()
		go func() { _ = op.Wait() }()
		return nil, ctx.Err()
	case <-execCtx.Done():
		cancelExec()
		cleanupPipes()
		go func() { _ = op.Wait() }()
		return nil, errors.New("console is shutting down")
	}
	if hs.err != nil {
		cancelExec()
		cleanupPipes()
		go func() { _ = op.Wait() }()
		return nil, fmt.Errorf("sftp handshake failed: %w", hs.err)
	}
	root := path.Clean(hs.wd)
	if root == "." || root == "/" || !strings.HasPrefix(root, "/home/") {
		_ = hs.client.Close()
		cancelExec()
		cleanupPipes()
		go func() { _ = op.Wait() }()
		return nil, errors.New("the SFTP home directory is not available on this instance")
	}

	sess := &sftpSession{
		client: hs.client, root: root, userID: user.ID, username: user.Username,
		token: token, tokenHash: hash, instance: name,
		cancel: cancelExec, closePipes: cleanupPipes, lastUsed: time.Now(),
		uploads: map[uint64]*sftpUpload{}, downloads: map[uint64]*sftp.File{},
	}
	s.sftpMu.Lock()
	s.sftpSessions[hash] = sess
	s.sftpMu.Unlock()

	// If the guest process ends on its own, drop the session so a reconnect
	// starts fresh instead of reusing a dead client.
	go func() {
		_ = op.Wait()
		s.sftpMu.Lock()
		if current, ok := s.sftpSessions[hash]; ok && current == sess {
			delete(s.sftpSessions, hash)
		}
		s.sftpMu.Unlock()
		sess.shutdown()
		cleanupPipes()
	}()
	return sess, nil
}

// closeSFTP tears down one token's connection.
func (s *Server) closeSFTP(tokenHash string) {
	s.sftpMu.Lock()
	sess := s.sftpSessions[tokenHash]
	delete(s.sftpSessions, tokenHash)
	s.sftpMu.Unlock()
	if sess != nil {
		sess.shutdown()
	}
}

// revokeSFTPUser closes every SFTP connection belonging to a user. Logout,
// session revocation and password changes all end up here so an already-open
// panel cannot outlive the console session that created it.
func (s *Server) revokeSFTPUser(userID int64, exceptHash string) {
	s.sftpMu.Lock()
	var dead []*sftpSession
	for hash, sess := range s.sftpSessions {
		if sess.userID == userID && hash != exceptHash {
			dead = append(dead, sess)
			delete(s.sftpSessions, hash)
		}
	}
	s.sftpMu.Unlock()
	for _, sess := range dead {
		sess.shutdown()
	}
}

// revokeSFTPHandle closes the panel tied to one public session handle.
func (s *Server) revokeSFTPHandle(handle string) {
	s.sftpMu.Lock()
	var dead []*sftpSession
	for hash, sess := range s.sftpSessions {
		if database.SessionHandle(hash) == handle {
			dead = append(dead, sess)
			delete(s.sftpSessions, hash)
		}
	}
	s.sftpMu.Unlock()
	for _, sess := range dead {
		sess.shutdown()
	}
}

// --- wire protocol ---------------------------------------------------------

type sftpMessage struct {
	ID      uint64      `json:"id,omitempty"`
	Op      string      `json:"op"`
	OK      bool        `json:"ok,omitempty"`
	Error   string      `json:"error,omitempty"`
	Path    string      `json:"path,omitempty"`
	Dest    string      `json:"dest,omitempty"`
	Paths   []string    `json:"paths,omitempty"`
	Entries []sftpEntry `json:"entries,omitempty"`
	CWD     string      `json:"cwd,omitempty"`
	Size    int64       `json:"size,omitempty"`
	Offset  int64       `json:"offset,omitempty"`
	Length  int         `json:"length,omitempty"`
}

type sftpEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Link  bool   `json:"link"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
	MTime string `json:"mtime"`
}

// sftpConn bundles the socket and its write mutex; the watchdog and the
// reader goroutine can both send frames without interleaving them.
type sftpConn struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	done chan struct{}
}

func (c *sftpConn) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(sftpWriteTimeout))
	return c.ws.WriteJSON(v)
}

func (c *sftpConn) sendError(err error) {
	_ = c.send(sftpMessage{Op: "error", OK: false, Error: friendlySFTPError(err)})
}

func (c *sftpConn) sendChunk(id uint64, offset int64, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := make([]byte, 16+len(data))
	binary.BigEndian.PutUint64(frame[0:8], id)
	binary.BigEndian.PutUint64(frame[8:16], uint64(offset))
	copy(frame[16:], data)
	_ = c.ws.SetWriteDeadline(time.Now().Add(sftpWriteTimeout))
	return c.ws.WriteMessage(websocket.BinaryMessage, frame)
}

func friendlySFTPError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errSFTPPath) {
		return "that path is outside this session's home directory"
	}
	if errors.Is(err, os.ErrNotExist) {
		return "that file no longer exists"
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission denied"
	}
	return err.Error()
}

// handleSFTPSocket bridges the browser panel to a per-session SFTP client.
func (s *Server) handleSFTPSocket(w http.ResponseWriter, r *http.Request) {
	if !s.App.Cfg.TerminalEnabled {
		http.Error(w, "The web terminal is disabled on this console.", http.StatusForbidden)
		return
	}
	if err := checkWriteAccess(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if !s.consoleRequestAllowed(r) {
		http.Error(w, "request origin or host is not allowed", http.StatusForbidden)
		return
	}
	user := userFrom(r)
	if user == nil {
		http.Error(w, "no console session", http.StatusUnauthorized)
		return
	}
	if !s.consoleAcquire(user.ID) {
		http.Error(w, "too many live console sessions — close another window first", http.StatusServiceUnavailable)
		return
	}
	released := false
	defer func() {
		if !released {
			s.consoleRelease(user.ID)
		}
	}()

	ws, err := consoleUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(sftpReadLimit)
	conn := &sftpConn{ws: ws, done: make(chan struct{})}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			close(conn.done)
			_ = ws.Close()
			s.closeSFTP(auth.HashToken(tokenFrom(r)))
			s.consoleRelease(user.ID)
			released = true
		})
	}
	defer cleanup()

	sess, err := s.openSFTP(r.Context(), r, r.PathValue("name"))
	if err != nil {
		conn.sendError(err)
		return
	}
	sess.mu.Lock()
	sess.wsCancel = func() { _ = ws.Close() }
	sess.lastUsed = time.Now()
	sess.mu.Unlock()
	_ = conn.send(sftpMessage{Op: "ready", OK: true, CWD: sess.root})

	// Watchdog: reject a connection whose console session was revoked, expired,
	// or whose panel has been idle for too long.
	go func() {
		ticker := time.NewTicker(sftpSessionTick)
		defer ticker.Stop()
		for {
			select {
			case <-conn.done:
				return
			case <-ticker.C:
				if _, aerr := s.App.Auth.Authenticate(sess.token); aerr != nil {
					_ = conn.send(sftpMessage{Op: "error", OK: false, Error: "console session expired"})
					cleanup()
					return
				}
				if sess.idle() > sftpIdleTimeout {
					_ = conn.send(sftpMessage{Op: "error", OK: false, Error: "file panel session timed out"})
					cleanup()
					return
				}
			}
		}
	}()

	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		sess.touch()
		if mt == websocket.BinaryMessage {
			s.handleSFTPWrite(conn, sess, data)
			continue
		}
		var msg sftpMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			conn.sendError(errors.New("invalid file panel message"))
			continue
		}
		if err := s.handleSFTPControl(conn, sess, msg); err != nil {
			conn.send(sftpMessage{ID: msg.ID, Op: msg.Op, OK: false, Error: friendlySFTPError(err)})
		} else if !sftpOpHasResponse(msg.Op) {
			// Mutations return only an error from their handler; the browser
			// still needs a completion ack to settle the request.
			conn.send(sftpMessage{ID: msg.ID, Op: msg.Op, OK: true})
		}
	}
}

// handleSFTPWrite queues a binary chunk: [8-byte id][8-byte offset][data].
func (s *Server) handleSFTPWrite(conn *sftpConn, sess *sftpSession, frame []byte) {
	if len(frame) < 16 {
		conn.sendError(errors.New("short file chunk"))
		return
	}
	id := binary.BigEndian.Uint64(frame[0:8])
	offset := int64(binary.BigEndian.Uint64(frame[8:16]))
	sess.transfers.Lock()
	u := sess.uploads[id]
	sess.transfers.Unlock()
	if u == nil {
		conn.send(sftpMessage{ID: id, Op: "write", OK: false, Offset: offset, Error: "upload was not started"})
		return
	}
	ok, full := u.enqueue(sftpWriteJob{offset: offset, data: frame[16:]})
	if !ok {
		msg := "upload was not started"
		if full {
			msg = "upload queue full"
		}
		conn.send(sftpMessage{ID: id, Op: "write", OK: false, Offset: offset, Error: msg})
	}
}

// sftpOpHasResponse lists operations whose handler sends its own JSON reply.
// "read" deliberately is not in the list: it streams a binary chunk, and the
// browser still needs a JSON ack to settle its request (the generic one from
// the reader loop).
func sftpOpHasResponse(op string) bool {
	switch op {
	case "ping", "list", "upload_begin", "upload_end", "download_begin":
		return true
	}
	return false
}

func (s *Server) handleSFTPControl(conn *sftpConn, sess *sftpSession, msg sftpMessage) error {
	switch msg.Op {
	case "ping":
		return conn.send(sftpMessage{ID: msg.ID, Op: "pong", OK: true})
	case "list":
		dir, err := sess.resolve(msg.Path)
		if err != nil {
			return err
		}
		infos, err := sess.client.ReadDir(dir)
		if err != nil {
			return err
		}
		entries := make([]sftpEntry, 0, len(infos))
		for _, info := range infos {
			entries = append(entries, sftpEntry{
				Name:  info.Name(),
				Path:  path.Join(dir, info.Name()),
				Dir:   info.IsDir(),
				Link:  info.Mode()&os.ModeSymlink != 0,
				Size:  info.Size(),
				Mode:  info.Mode().String(),
				MTime: info.ModTime().Format(time.RFC3339),
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Dir != entries[j].Dir {
				return entries[i].Dir
			}
			return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
		})
		return conn.send(sftpMessage{ID: msg.ID, Op: "list", OK: true, Entries: entries, CWD: dir})

	case "mkdir":
		p, err := sess.resolveChild(msg.Path, msg.Dest)
		if err != nil {
			return err
		}
		return sess.client.Mkdir(p)

	case "rename":
		src, err := sess.resolve(msg.Path)
		if err != nil {
			return err
		}
		// Dest is the full target path, so a rename can also move a file.
		dst, err := sess.resolve(msg.Dest)
		if err != nil {
			return err
		}
		if src == dst {
			return nil
		}
		return sess.client.Rename(src, dst)

	case "remove":
		if len(msg.Paths) > sftpMaxBatchItems {
			return errors.New("too many items selected")
		}
		paths, err := sess.resolveAll(msg.Paths)
		if err != nil {
			return err
		}
		for _, p := range paths {
			if p == sess.root {
				return errors.New("the home folder cannot be deleted")
			}
			if err := sess.client.RemoveAll(p); err != nil {
				return err
			}
		}
		return nil

	case "copy", "move":
		if len(msg.Paths) > sftpMaxBatchItems {
			return errors.New("too many items selected")
		}
		dest, err := sess.resolve(msg.Dest)
		if err != nil {
			return err
		}
		sources, err := sess.resolveAll(msg.Paths)
		if err != nil {
			return err
		}
		for _, src := range sources {
			if withinRoot(src, dest) && src != sess.root {
				return errors.New("cannot place a folder inside itself")
			}
			target := path.Join(dest, path.Base(src))
			if msg.Op == "move" {
				if err := sess.client.Rename(src, target); err != nil {
					return err
				}
			} else if err := sess.copyRecursive(src, target); err != nil {
				return err
			}
		}
		return nil

	case "upload_begin":
		sess.transfers.Lock()
		activeUploads := len(sess.uploads)
		sess.transfers.Unlock()
		if activeUploads >= sftpMaxTransfers {
			return errors.New("too many active uploads")
		}
		p, err := sess.resolve(msg.Path)
		if err != nil {
			return err
		}
		f, err := sess.client.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			return err
		}
		u := startSFTPUpload(conn, msg.ID, f)
		sess.transfers.Lock()
		old := sess.uploads[msg.ID]
		sess.uploads[msg.ID] = u
		sess.transfers.Unlock()
		if old != nil {
			old.abort()
		}
		return conn.send(sftpMessage{ID: msg.ID, Op: "upload_begin", OK: true, Size: msg.Size})

	case "upload_end":
		sess.transfers.Lock()
		u := sess.uploads[msg.ID]
		if u != nil {
			delete(sess.uploads, msg.ID)
		}
		sess.transfers.Unlock()
		if u == nil {
			return errors.New("upload was not started")
		}
		if err := u.finish(); err != nil {
			return err
		}
		return conn.send(sftpMessage{ID: msg.ID, Op: "upload_end", OK: true})

	case "download_begin":
		sess.transfers.Lock()
		active := len(sess.downloads)
		sess.transfers.Unlock()
		if active >= sftpMaxTransfers {
			return errors.New("too many active downloads")
		}
		p, err := sess.resolve(msg.Path)
		if err != nil {
			return err
		}
		f, err := sess.client.Open(p)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		sess.transfers.Lock()
		if old := sess.downloads[msg.ID]; old != nil {
			_ = old.Close()
		}
		sess.downloads[msg.ID] = f
		sess.transfers.Unlock()

		// Stream the whole file in one direction. Chunks carry their own id
		// and offset, so several downloads can interleave safely, and the
		// browser never needs a round trip per chunk.
		if err := conn.send(sftpMessage{ID: msg.ID, Op: "download_begin", OK: true, Size: info.Size()}); err != nil {
			sess.transfers.Lock()
			if current := sess.downloads[msg.ID]; current == f {
				delete(sess.downloads, msg.ID)
			}
			sess.transfers.Unlock()
			_ = f.Close()
			return err
		}
		go s.streamDownload(conn, sess, msg.ID, f, info.Size())
		return nil

	case "read":
		sess.transfers.Lock()
		f := sess.downloads[msg.ID]
		sess.transfers.Unlock()
		if f == nil {
			return errors.New("download was not started")
		}
		length := msg.Length
		if length <= 0 || length > sftpMaxChunk {
			length = sftpMaxChunk
		}
		buf := make([]byte, length)
		n, err := f.ReadAt(buf, msg.Offset)
		if err != nil && err != io.EOF {
			return err
		}
		return conn.sendChunk(msg.ID, msg.Offset, buf[:n])

	case "download_end":
		sess.transfers.Lock()
		f := sess.downloads[msg.ID]
		if f != nil {
			delete(sess.downloads, msg.ID)
		}
		sess.transfers.Unlock()
		if f == nil {
			return nil
		}
		return f.Close()

	default:
		return fmt.Errorf("unknown file panel operation %q", msg.Op)
	}
}

// resolveChild resolves a name inside the directory named by base. It is used
// by mkdir/rename where the browser sends a directory and a new name.
func (sess *sftpSession) resolveChild(base, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || len(name) > sftpMaxNameLen ||
		strings.ContainsAny(name, "/\\\x00") {
		return "", errors.New("invalid name")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("invalid name")
		}
	}
	dir, err := sess.resolve(base)
	if err != nil {
		return "", err
	}
	return path.Join(dir, name), nil
}

func (sess *sftpSession) resolveAll(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		resolved, err := sess.resolve(p)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

// streamDownload pushes a file to the browser as tagged chunks followed by a
// single completion message. Running it in its own goroutine keeps a slow
// client from blocking the socket reader for other operations.
func (s *Server) streamDownload(conn *sftpConn, sess *sftpSession, id uint64, f *sftp.File, size int64) {
	finished := func() {
		sess.transfers.Lock()
		if current := sess.downloads[id]; current == f {
			delete(sess.downloads, id)
		}
		sess.transfers.Unlock()
		_ = f.Close()
	}
	defer finished()

	buf := make([]byte, sftpMaxChunk)
	failed := false
	for offset := int64(0); offset < size; {
		select {
		case <-conn.done:
			return
		default:
		}
		n, err := f.ReadAt(buf, offset)
		if n > 0 {
			if cerr := conn.sendChunk(id, offset, buf[:n]); cerr != nil {
				return
			}
			offset += int64(n)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				failed = true
				_ = conn.send(sftpMessage{ID: id, Op: "download_error", OK: false, Error: friendlySFTPError(err)})
			}
			break
		}
		if n == 0 {
			// A short read that makes no progress would spin forever.
			failed = true
			_ = conn.send(sftpMessage{ID: id, Op: "download_error", OK: false, Error: "file read made no progress"})
			break
		}
	}
	// Only send completion if no error was already sent; sending download_done
	// after download_error would cause the client to try to save a broken file.
	if !failed {
		_ = conn.send(sftpMessage{ID: id, Op: "download_done", OK: true, Size: size})
	}
}

func (sess *sftpSession) copyRecursive(src, dst string) error {
	info, err := sess.client.Stat(src)
	if err != nil {
		return err
	}
	if withinRoot(src, dst) && src != sess.root {
		return errors.New("cannot copy a folder into itself")
	}
	if info.IsDir() {
		if err := sess.client.MkdirAll(dst); err != nil {
			return err
		}
		children, err := sess.client.ReadDir(src)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := sess.copyRecursive(path.Join(src, child.Name()), path.Join(dst, child.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := sess.client.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := sess.client.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func sanitizeDownloadFilename(name string) string {
	base := path.Base(name)
	var b strings.Builder
	for _, r := range base {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r == '/' {
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "download"
	}
	return b.String()
}

// handleSFTPDownload streams a file directly over HTTP using sftp.File.WriteTo
// (concurrent pipelined SFTP reads) so downloads bypass WebSocket framing.
func (s *Server) handleSFTPDownload(w http.ResponseWriter, r *http.Request) {
	if !s.App.Cfg.TerminalEnabled {
		http.Error(w, "The web terminal is disabled on this console.", http.StatusForbidden)
		return
	}
	if err := checkWriteAccess(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if userFrom(r) == nil {
		http.Error(w, "no console session", http.StatusUnauthorized)
		return
	}
	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}

	sess, err := s.openSFTP(r.Context(), r, r.PathValue("name"))
	if err != nil {
		http.Error(w, friendlySFTPError(err), http.StatusBadGateway)
		return
	}
	sess.touch()

	p, err := sess.resolve(reqPath)
	if err != nil {
		http.Error(w, friendlySFTPError(err), http.StatusForbidden)
		return
	}
	f, err := sess.client.Open(p)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		} else if errors.Is(err, os.ErrPermission) {
			status = http.StatusForbidden
		}
		http.Error(w, friendlySFTPError(err), status)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, friendlySFTPError(err), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "cannot download a directory", http.StatusBadRequest)
		return
	}

	// Disable the server's fixed write deadline for large file streams.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	filename := sanitizeDownloadFilename(p)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")

	if _, err := io.Copy(w, f); err != nil {
		s.log.Debug("sftp http download ended early", "path", p, "err", err)
	}
}
