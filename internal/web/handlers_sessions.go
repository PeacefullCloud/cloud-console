package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/database"
)

// sessionViews lists a user's live sessions, marking the one that made the
// current request.
func (s *Server) sessionViews(r *http.Request, userID int64) []sessionView {
	list, err := s.App.DB.ListUserSessions(userID)
	if err != nil {
		s.log.Warn("could not list sessions", "err", err)
		return nil
	}
	current := ""
	if token := tokenFrom(r); token != "" {
		current = database.SessionHandle(auth.HashToken(token))
	}
	out := make([]sessionView, 0, len(list))
	for _, item := range list {
		out = append(out, sessionView{
			ID:        item.ID,
			IP:        item.IP,
			Device:    deviceLabel(item.UserAgent),
			CreatedAt: item.CreatedAt,
			ExpiresAt: item.ExpiresAt,
			Current:   current != "" && item.ID == current,
		})
	}
	return out
}

// deviceLabel turns a User-Agent header into a short "Browser on OS" label.
func deviceLabel(ua string) string {
	if ua == "" {
		return "Unknown device"
	}
	browser := "Browser"
	// Order matters: Edge and Chrome both mention Safari, Chrome mentions
	// nothing of Firefox, and so on.
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"},
		{"Chrome/", "Chrome"}, {"Safari/", "Safari"}, {"curl/", "curl"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	system := ""
	for _, o := range []struct{ token, name string }{
		{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iOS"},
		{"Windows", "Windows"}, {"Mac OS X", "macOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			system = o.name
			break
		}
	}
	if system == "" {
		return browser
	}
	return browser + " on " + system
}

// serveSessionsFragments re-renders the sessions card for HTMX requests.
func (s *Server) serveSessionsFragments(w http.ResponseWriter, r *http.Request, actionErr error, notice string) {
	data := sessionsFragmentData{CSRF: csrfFrom(r)}
	if actionErr != nil {
		data.Error = friendlyError(actionErr)
	} else {
		data.Notice = notice
	}
	if user := userFrom(r); user != nil {
		data.Sessions = s.sessionViews(r, user.ID)
	}
	if err := s.Renderer.RenderPartial(w, "settings", "sessions_response", data); err != nil {
		s.log.Error("render sessions card", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// handleSessionRevoke signs one of the caller's own sessions out.
func (s *Server) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	handle := r.PathValue("id")

	var err error
	notice := "Device signed out."
	if token := tokenFrom(r); token != "" && database.SessionHandle(auth.HashToken(token)) == handle {
		err = errors.New("this is the device you are using — use Sign out instead")
	} else if ok, derr := s.App.DB.DeleteUserSessionByHandle(user.ID, handle); derr != nil {
		err = derr
	} else if !ok {
		err = errors.New("that session no longer exists")
	}
	s.App.Activity.Record(user.Username, "Revoke session", user.Username, "", err)

	if isHTMX(r) {
		s.serveSessionsFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, notice)
}

// handleSessionsRevokeOthers signs the caller out of every other device.
func (s *Server) handleSessionsRevokeOthers(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	n, err := s.App.DB.DeleteOtherUserSessions(user.ID, auth.HashToken(tokenFrom(r)))
	s.App.Activity.Record(user.Username, "Revoke other sessions", user.Username, "", err)

	notice := "No other devices were signed in."
	if n == 1 {
		notice = "Signed out 1 other device."
	} else if n > 1 {
		notice = "Signed out " + strconv.FormatInt(n, 10) + " other devices."
	}
	if isHTMX(r) {
		s.serveSessionsFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, notice)
}

// handleUserPasswordReset lets an admin set a new password for another user.
func (s *Server) handleUserPasswordReset(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.PathValue("id"), 0))
	next := r.FormValue("new_password")

	label := ""
	var err error
	notice := ""
	if aerr := checkAdminAccess(r); aerr != nil {
		err = aerr
	} else if id == actor.ID {
		err = errors.New("use \"Your password\" to change your own password")
	} else if target, gerr := s.App.DB.GetUser(id); gerr != nil {
		err = gerr
	} else {
		label = target.Username
		if err = s.App.Auth.ResetPassword(target.ID, next); err == nil {
			notice = "Password for " + target.Username + " reset. They were signed out everywhere."
		}
	}
	s.App.Activity.Record(actor.Username, "Reset user password", label, "", err)

	if isHTMX(r) {
		s.serveUsersFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, notice)
}
