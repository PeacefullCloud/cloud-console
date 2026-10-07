package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/peaceful/cloud-console/internal/auth"
)

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	kind, message := s.takeFlash(w, r)
	data := loginData{
		Title:  "Sign in",
		CSRF:   csrfFrom(r),
		Next:   safeNext(r.URL.Query().Get("next")),
		Notice: message,
	}
	if kind == "err" {
		data.Error = message
		data.Notice = ""
	}

	if err := s.Renderer.RenderStandalone(w, "templates/login.html", "login_page", data); err != nil {
		s.log.Error("render login page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	next := safeNext(r.FormValue("next"))

	token, user, err := s.App.Auth.Login(username, password, clientIP(r), r.UserAgent())
	if err != nil {
		s.App.Activity.Record(username, "Sign in", "", "from "+clientIP(r), err)

		kind := "err"
		message := "Incorrect username or password."
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			message = "Sign-in failed: " + err.Error()
		}
		s.setFlash(w, kind, message)

		target := "/login"
		if next != "" {
			target += "?next=" + url.QueryEscape(next)
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}

	// Second step when two-factor is on: the password passed but no session
	// exists yet. The challenge id carries the pending login for 5 minutes.
	if user.TOTPEnabled {
		challenge, err := s.App.Auth.BeginTOTPChallenge(user.ID)
		if err != nil {
			s.setFlash(w, "err", "Sign-in failed: "+err.Error())
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if err := s.Renderer.RenderStandalone(w, "templates/login.html", "totp_page", totpData{
			Title:     "Two-factor authentication",
			CSRF:      csrfFrom(r),
			Challenge: challenge,
			Username:  user.Username,
			Next:      next,
		}); err != nil {
			s.log.Error("render totp page", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	s.App.Activity.Record(user.Username, "Sign in", "", "from "+clientIP(r), nil)
	s.finishPasswordLogin(w, r, token, user.Username, next)
}

// handleLoginTOTPSubmit verifies the authenticator code and opens the session.
func (s *Server) handleLoginTOTPSubmit(w http.ResponseWriter, r *http.Request) {
	challenge := strings.TrimSpace(r.FormValue("challenge"))
	code := strings.TrimSpace(r.FormValue("code"))
	next := safeNext(r.FormValue("next"))

	userID, ok := s.App.Auth.FinishTOTPChallenge(challenge)
	if !ok {
		s.setFlash(w, "err", "That sign-in expired — please sign in again.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	user, err := s.App.DB.GetUser(userID)
	if err != nil {
		s.setFlash(w, "err", "That sign-in expired — please sign in again.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if !user.TOTPEnabled || !auth.VerifyTOTPCode(user.TOTPSecret, code) {
		s.App.Activity.Record(user.Username, "Sign in (2FA)", "", "from "+clientIP(r), errors.New("invalid code"))
		if err := s.Renderer.RenderStandalone(w, "templates/login.html", "totp_page", totpData{
			Title:     "Two-factor authentication",
			CSRF:      csrfFrom(r),
			Challenge: mustBeginChallenge(s, user.ID, w, r),
			Username:  user.Username,
			Next:      next,
			Error:     "Incorrect code. Try the current code from your app.",
		}); err != nil {
			s.log.Error("render totp page", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	token, err := s.App.Auth.OpenSession(user, clientIP(r), r.UserAgent())
	if err != nil {
		s.setFlash(w, "err", "Sign-in failed: "+err.Error())
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	s.App.Activity.Record(user.Username, "Sign in (2FA)", "", "from "+clientIP(r), nil)
	s.finishPasswordLogin(w, r, token, user.Username, next)
}

// mustBeginChallenge issues a replacement challenge after a wrong code so the
// form stays usable. The old challenge was already consumed.
func mustBeginChallenge(s *Server, userID int64, w http.ResponseWriter, r *http.Request) string {
	challenge, err := s.App.Auth.BeginTOTPChallenge(userID)
	if err != nil {
		s.setFlash(w, "err", "Sign-in failed: "+err.Error())
		return ""
	}
	return challenge
}

// finishPasswordLogin stores the session cookie and lands on next (or /).
func (s *Server) finishPasswordLogin(w http.ResponseWriter, r *http.Request, token, username, next string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.App.Cfg.SecureCookies,
		MaxAge:   int(time.Duration(s.App.Cfg.SessionTTL) * time.Hour / time.Second),
	})

	// Rotate the CSRF token on privilege change.
	s.clearCookie(w, csrfCookie)

	target := next
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := tokenFrom(r); token != "" {
		if err := s.App.Auth.Logout(token); err != nil {
			s.log.Warn("could not close session", "err", err)
		}
	}

	if user := userFrom(r); user != nil {
		s.App.Activity.Record(user.Username, "Sign out", "", "", nil)
	}

	s.clearCookie(w, sessionCookie)
	s.setFlash(w, "ok", "You have been signed out.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext only allows same-site relative redirect targets.
func safeNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return ""
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return ""
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" {
		return ""
	}
	return next
}

// clientIP extracts the caller address, trusting the reverse proxy headers the
// console's own Caddy sets.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, ok := strings.Cut(forwarded, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}

	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
