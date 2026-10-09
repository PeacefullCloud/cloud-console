package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
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

	user, err := s.App.Auth.VerifyCredentials(username, password, s.clientIP(r))
	if err != nil {
		s.App.Activity.Record(username, "Sign in", "", "from "+s.clientIP(r), err)

		kind := "err"
		message := "Incorrect username or password."
		var lockout *auth.LockoutError
		switch {
		case errors.As(err, &lockout):
			message = lockout.Error()
		case !errors.Is(err, auth.ErrInvalidCredentials):
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

	// No session exists yet. Accounts restricted to single sign-on stop here,
	// before a second factor is even asked for.
	if !sso.PasswordAllowed(user.AuthMethod) {
		s.denyPasswordSignIn(w, r, user.Username, "Sign in")
		return
	}

	// Second step when two-factor is on: the password passed but no session
	// exists yet. The challenge id carries the pending login for 5 minutes.
	if user.TOTPEnabled {
		challenge, err := s.App.Auth.BeginTOTPChallenge(user.ID)
		if err != nil {
			message := "Sign-in failed: " + err.Error()
			var lockout *auth.LockoutError
			if errors.As(err, &lockout) {
				message = lockout.Error()
			}
			if isHTMX(r) {
				s.serveLoginFlash(w, message)
				return
			}
			s.setFlash(w, "err", message)
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

	token, err := s.App.Auth.OpenSession(user, s.clientIP(r), r.UserAgent())
	if err != nil {
		if isHTMX(r) {
			s.serveLoginFlash(w, "Sign-in failed: "+err.Error())
			return
		}
		s.setFlash(w, "err", "Sign-in failed: "+err.Error())
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.App.Activity.Record(user.Username, "Sign in", "", "from "+s.clientIP(r), nil)
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

// serveTOTPForm re-renders the code form with the same challenge plus an error
// flash, so a mistyped code retries in place without a reload.
func (s *Server) serveTOTPForm(w http.ResponseWriter, r *http.Request, challenge, username, next, message string) {
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

	restart := func(message string) {
		s.setFlash(w, "err", message)
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}

	userID, ok := s.App.Auth.PeekTOTPChallenge(challenge)
	if !ok {
		restart("That sign-in expired — please sign in again.")
		return
	}
	user, err := s.App.DB.GetUser(userID)
	if err != nil {
		restart("That sign-in expired — please sign in again.")
		return
	}

	usedRecovery, err := s.App.Auth.VerifySecondFactor(challenge, user.ID, user.TOTPSecret, user.TOTPEnabled, code)
	if err != nil {
		s.App.Activity.Record(user.Username, "Sign in (2FA)", "", "from "+s.clientIP(r), err)

		var lockout *auth.LockoutError
		switch {
		case errors.Is(err, auth.ErrInvalidCode):
			message := "Incorrect code. Try the current code from your app, or a recovery code."
			if isHTMX(r) {
				s.serveTOTPForm(w, r, challenge, user.Username, next, message)
				return
			}
			if err := s.Renderer.RenderStandalone(w, "templates/login.html", "totp_page", totpData{
				Title:     "Two-factor authentication",
				CSRF:      csrfFrom(r),
				Challenge: challenge,
				Username:  user.Username,
				Next:      next,
				Error:     message,
			}); err != nil {
				s.log.Error("render totp page", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		case errors.As(err, &lockout):
			restart(lockout.Error())
		case errors.Is(err, auth.ErrChallengeExhausted):
			restart("Too many incorrect codes — please sign in again.")
		default:
			restart("That sign-in expired — please sign in again.")
		}
		return
	}

	if usedRecovery {
		s.App.Activity.Record(user.Username, "Sign in with recovery code", "",
			strconv.Itoa(s.App.Auth.RecoveryCodesLeft(user.ID))+" left", nil)
	}

	if !sso.PasswordAllowed(user.AuthMethod) {
		s.denyPasswordSignIn(w, r, user.Username, "Sign in (2FA)")
		return
	}

	token, err := s.App.Auth.OpenSession(user, s.clientIP(r), r.UserAgent())
	if err != nil {
		if isHTMX(r) {
			s.serveLoginFlash(w, "Sign-in failed: "+err.Error())
			return
		}
		s.setFlash(w, "err", "Sign-in failed: "+err.Error())
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	s.App.Activity.Record(user.Username, "Sign in (2FA)", "", "from "+s.clientIP(r), nil)
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

// denyPasswordSignIn refuses a password sign-in for an account that is limited
// to single sign-on.
func (s *Server) denyPasswordSignIn(w http.ResponseWriter, r *http.Request, username, action string) {
	const message = "This account uses single sign-on — use a sign-in button below."
	s.App.Activity.Record(username, action, "", "account uses single sign-on", errors.New("password sign-in disabled"))
	if isHTMX(r) {
		s.serveLoginFlash(w, message)
		return
	}
	s.setFlash(w, "err", message)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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
	// Browsers read a backslash as a slash, and strip tabs and newlines, so
	// "/\evil.example" and "/\t/evil.example" both leave the site.
	if strings.ContainsAny(next, "\\\t\r\n") {
		return ""
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" {
		return ""
	}
	return next
}
