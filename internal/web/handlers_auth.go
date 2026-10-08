package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/sso"
)

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	kind, message := s.takeFlash(w, r)
	providers, err := s.SSO.PublicProviders()
	if err != nil {
		s.log.Warn("could not list sso providers", "err", err)
	}
	data := loginData{
		Title:     "Sign in",
		CSRF:      csrfFrom(r),
		Next:      safeNext(r.URL.Query().Get("next")),
		Notice:    message,
		Providers: providers,
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
		if isHTMX(r) {
			s.serveLoginFlash(w, message)
			return
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
			if isHTMX(r) {
				s.serveLoginFlash(w, "Sign-in failed: "+err.Error())
				return
			}
			s.setFlash(w, "err", "Sign-in failed: "+err.Error())
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if isHTMX(r) {
			// The code form lives at its own URL; navigating there is a new
			// page, not a reload of this one.
			target := "/login/totp?challenge=" + url.QueryEscape(challenge)
			if next != "" {
				target += "&next=" + url.QueryEscape(next)
			}
			w.Header().Set("HX-Redirect", target)
			w.WriteHeader(http.StatusOK)
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
	if !sso.PasswordAllowed(user.AuthMethod) {
		// The session was already created above; close it so the denial
		// does not leave a usable session behind.
		_ = s.App.Auth.Logout(token)
		s.App.Activity.Record(user.Username, "Sign in", "", "account uses single sign-on", errors.New("password sign-in disabled"))
		if isHTMX(r) {
			s.serveLoginFlash(w, "This account uses single sign-on — use a sign-in button below.")
			return
		}
		s.setFlash(w, "err", "This account uses single sign-on — use a sign-in button below.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if isHTMX(r) {
		s.setSessionCookie(w, token)
		target := next
		if target == "" {
			target = "/"
		}
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.finishPasswordLogin(w, r, token, user.Username, next)
}

// handleTOTPPage serves the code form at its own URL so the password step
// can hand off to it without re-rendering.
func (s *Server) handleTOTPPage(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	challenge := strings.TrimSpace(r.URL.Query().Get("challenge"))
	next := safeNext(r.URL.Query().Get("next"))

	userID, ok := s.App.Auth.PeekTOTPChallenge(challenge)
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
}

// serveLoginFlash renders a flash-only out-of-band swap for the sign-in
// pages, whose forms use hx-swap="none".
func (s *Server) serveLoginFlash(w http.ResponseWriter, message string) {
	data := loginData{Error: message}
	if err := s.Renderer.RenderStandalone(w, "templates/login.html", "login_flash_oob", data); err != nil {
		s.log.Error("render login flash", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// serveTOTPForm renders the code form with a fresh challenge plus an error
// flash, so a mistyped code retries in place without a reload.
func (s *Server) serveTOTPForm(w http.ResponseWriter, r *http.Request, userID int64, username, next, message string) {
	challenge, err := s.App.Auth.BeginTOTPChallenge(userID)
	if err != nil {
		s.setFlash(w, "err", "Sign-in failed: "+err.Error())
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}

	data := totpData{
		CSRF:      csrfFrom(r),
		Challenge: challenge,
		Username:  username,
		Next:      next,
		Error:     message,
	}
	if err := s.Renderer.RenderStandalone(w, "templates/login.html", "totp_form_response", data); err != nil {
		s.log.Error("render totp form", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// handleLoginTOTPSubmit verifies the authenticator code and opens the session.
func (s *Server) handleLoginTOTPSubmit(w http.ResponseWriter, r *http.Request) {
	challenge := strings.TrimSpace(r.FormValue("challenge"))
	code := strings.TrimSpace(r.FormValue("code"))
	next := safeNext(r.FormValue("next"))

	userID, ok := s.App.Auth.FinishTOTPChallenge(challenge)
	if !ok {
		if isHTMX(r) {
			s.setFlash(w, "err", "That sign-in expired — please sign in again.")
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusOK)
			return
		}
		s.setFlash(w, "err", "That sign-in expired — please sign in again.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	user, err := s.App.DB.GetUser(userID)
	if err != nil {
		if isHTMX(r) {
			s.setFlash(w, "err", "That sign-in expired — please sign in again.")
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusOK)
			return
		}
		s.setFlash(w, "err", "That sign-in expired — please sign in again.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if !user.TOTPEnabled || !auth.VerifyTOTPCode(user.TOTPSecret, code) {
		s.App.Activity.Record(user.Username, "Sign in (2FA)", "", "from "+clientIP(r), errors.New("invalid code"))
		if isHTMX(r) {
			s.serveTOTPForm(w, r, user.ID, user.Username, next, "Incorrect code. Try the current code from your app.")
			return
		}
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
		if isHTMX(r) {
			s.serveLoginFlash(w, "Sign-in failed: "+err.Error())
			return
		}
		s.setFlash(w, "err", "Sign-in failed: "+err.Error())
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	s.App.Activity.Record(user.Username, "Sign in (2FA)", "", "from "+clientIP(r), nil)
	if !sso.PasswordAllowed(user.AuthMethod) {
		// Closed the loop opened at the password step: SSO-only accounts
		// cannot finish a password sign-in even with a valid TOTP code.
		_ = s.App.Auth.Logout(token)
		s.App.Activity.Record(user.Username, "Sign in (2FA)", "", "account uses single sign-on", errors.New("password sign-in disabled"))
		if isHTMX(r) {
			s.serveLoginFlash(w, "This account uses single sign-on — use a sign-in button below.")
			return
		}
		s.setFlash(w, "err", "This account uses single sign-on — use a sign-in button below.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if isHTMX(r) {
		s.setSessionCookie(w, token)
		target := next
		if target == "" {
			target = "/"
		}
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
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
	s.setSessionCookie(w, token)

	// Rotate the CSRF token on privilege change.
	s.clearCookie(w, csrfCookie)

	target := next
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// setSessionCookie stores the session token. The CSRF rotation in
// finishPasswordLogin is intentionally skipped for HTMX logins: the next
// full page load (after HX-Redirect) issues a fresh token anyway.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.App.Cfg.SecureCookies,
		MaxAge:   int(time.Duration(s.App.Cfg.SessionTTL) * time.Hour / time.Second),
	})
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
