package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/sso"
)

// --- SSO sign-in ------------------------------------------------------------

// handleSSOStart begins a sign-in at a provider. This is a plain navigation:
// the browser must leave for the IdP, so HTMX plays no part.
func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	providerID, err := strconv.ParseInt(r.PathValue("provider"), 10, 64)
	if err != nil || providerID <= 0 {
		s.setFlash(w, "err", "Unknown sign-in method.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	target, err := s.SSO.AuthURL(providerID, safeNext(r.URL.Query().Get("next")))
	if err != nil {
		s.log.Warn("sso start failed", "provider", providerID, "err", err)
		s.setFlash(w, "err", "Sign-in is unavailable: "+err.Error())
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, target, http.StatusFound)
}

// handleSSOCallback completes a sign-in returning from a provider.
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	providerID, err := strconv.ParseInt(r.PathValue("provider"), 10, 64)
	if err != nil || providerID <= 0 {
		s.setFlash(w, "err", "Unknown sign-in method.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	login, err := s.SSO.Callback(r.Context(), providerID, r.URL.Query().Get("code"), r.URL.Query().Get("state"))
	if err != nil {
		s.log.Warn("sso callback failed", "provider", providerID, "err", err)
		s.setFlash(w, "err", "Sign-in failed: "+err.Error())
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	user, err := s.resolveSSOUser(login)
	if err != nil {
		s.log.Warn("sso user resolution failed", "provider", providerID, "err", err)
		s.setFlash(w, "err", "Sign-in failed: "+friendlyError(err))
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if !sso.SSOAllowed(user.AuthMethod) {
		s.App.Activity.Record(user.Username, "Sign in (SSO)", login.ProviderName, "", errors.New("account uses password sign-in"))
		s.setFlash(w, "err", "This account uses password sign-in — ask an admin to change it.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	token, err := s.App.Auth.OpenSession(user, clientIP(r), r.UserAgent())
	if err != nil {
		s.setFlash(w, "err", "Sign-in failed: "+err.Error())
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	s.App.Activity.Record(user.Username, "Sign in (SSO)", login.ProviderName, "", nil)
	s.finishPasswordLogin(w, r, token, user.Username, safeNext(login.Next))
}

// resolveSSOUser maps a verified SSO login to a console user: a linked
// identity wins, then a username match (linked on the spot), otherwise a new
// account is provisioned with the provider's default role.
func (s *Server) resolveSSOUser(login *sso.Login) (*models.User, error) {
	if userID, err := s.App.DB.FindIdentityUser(login.ProviderID, login.Subject); err == nil {
		return s.App.DB.GetUser(userID)
	} else if !errors.Is(err, database.ErrNotFound) {
		return nil, err
	}

	if login.Username != "" {
		if existing, err := s.App.DB.GetUserByUsername(login.Username); err == nil {
			if err := s.App.DB.LinkIdentity(existing.ID, login.ProviderID, login.Subject); err != nil {
				return nil, err
			}
			return existing, nil
		} else if !errors.Is(err, database.ErrNotFound) {
			return nil, err
		}
	}

	username := sso.SuggestUsername(func(name string) bool {
		_, err := s.App.DB.GetUserByUsername(name)
		return err == nil
	}, login.Username)

	password, err := auth.RandomPassword(24)
	if err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}

	created, err := s.App.DB.CreateUser(username, hash, auth.RoleViewer)
	if err != nil {
		return nil, err
	}
	if err := s.App.DB.SetAuthMethod(created.ID, sso.MethodSSO); err != nil {
		return nil, err
	}
	if err := s.App.DB.LinkIdentity(created.ID, login.ProviderID, login.Subject); err != nil {
		return nil, err
	}
	return s.App.DB.GetUser(created.ID)
}

// --- SSO providers (Settings, admin only) -----------------------------------

// handleProviderSave creates or updates a provider from the Settings form.
func (s *Server) handleProviderSave(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.FormValue("id"), 0))
	input := sso.ProviderInput{
		ID:           id,
		Name:         r.FormValue("name"),
		Issuer:       r.FormValue("issuer"),
		ClientID:     r.FormValue("client_id"),
		ClientSecret: r.FormValue("client_secret"),
		ButtonLabel:  r.FormValue("button_label"),
		DefaultRole:  r.FormValue("default_role"),
		RequireMFA:   r.FormValue("require_mfa") != "",
		Enabled:      r.FormValue("enabled") != "",
	}
	// A new provider starts enabled; the checkbox is only present on edit.
	if id == 0 && r.FormValue("enabled") == "" {
		input.Enabled = true
	}

	if err := checkAdminAccess(r); err != nil {
		s.App.Activity.Record(actor.Username, "Save SSO provider", input.Name, "", err)
		if isHTMX(r) {
			s.serveSSOFragments(w, r, err, "")
			return
		}
		s.finish(w, r, "/settings", err, "")
		return
	}

	_, err := s.SSO.SaveProvider(input)
	s.App.Activity.Record(actor.Username, "Save SSO provider", input.Name, "", err)

	if isHTMX(r) {
		notice := ""
		if err == nil {
			notice = "Sign-in method " + input.Name + " saved."
		}
		s.serveSSOFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, "Sign-in method "+input.Name+" saved.")
}

// handleProviderDelete removes a provider. Providers with linked accounts
// are refused so nobody is silently locked out.
func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.PathValue("id"), 0))
	name := ""
	if p, err := s.App.DB.GetProvider(id); err == nil {
		name = p.Name
	}

	var err error
	if aerr := checkAdminAccess(r); aerr != nil {
		err = aerr
	} else {
		err = s.SSO.DeleteProvider(id)
	}
	s.App.Activity.Record(actor.Username, "Delete SSO provider", name, "", err)

	if isHTMX(r) {
		notice := ""
		if err == nil {
			notice = "Sign-in method " + name + " deleted."
		}
		s.serveSSOFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, "Sign-in method "+name+" deleted.")
}

// handleProviderToggle flips a provider on or off without opening the form.
func (s *Server) handleProviderToggle(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.PathValue("id"), 0))

	var err error
	if aerr := checkAdminAccess(r); aerr != nil {
		err = aerr
	} else if p, gerr := s.App.DB.GetProvider(id); gerr != nil {
		err = gerr
	} else if serr := s.SSO.SetEnabled(id, !p.Enabled); serr != nil {
		err = serr
	} else {
		state := "enabled"
		if p.Enabled {
			state = "disabled"
		}
		s.App.Activity.Record(actor.Username, "Toggle SSO provider", p.Name, state, nil)
		if isHTMX(r) {
			s.serveSSOFragments(w, r, nil, "Sign-in method "+p.Name+" "+state+".")
			return
		}
		s.succeed(w, r, "/settings", "Sign-in method "+p.Name+" "+state+".")
		return
	}

	s.App.Activity.Record(actor.Username, "Toggle SSO provider", "", "", err)
	if isHTMX(r) {
		s.serveSSOFragments(w, r, err, "")
		return
	}
	s.finish(w, r, "/settings", err, "")
}

// serveSSOFragments renders the HTMX response for provider operations: the
// fresh provider list plus an out-of-band flash.
func (s *Server) serveSSOFragments(w http.ResponseWriter, r *http.Request, actionErr error, notice string) {
	views, err := s.SSO.ProviderViews()
	if err != nil {
		actionErr = err
	}

	data := ssoFragmentData{
		CSRF:      csrfFrom(r),
		User:      userFrom(r),
		Providers: views,
		SSOKeySet: s.App.Cfg.SSOKeySet(),
	}
	if actionErr != nil {
		data.Error = friendlyError(actionErr)
	} else {
		data.Notice = notice
	}

	if err := s.Renderer.RenderPartial(w, "settings", "sso_response", data); err != nil {
		s.log.Error("render sso providers", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// --- per-user sign-in method (Settings, admin only) -------------------------

// handleUserMethod changes which sign-in paths an account may use.
func (s *Server) handleUserMethod(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.PathValue("id"), 0))
	method := strings.TrimSpace(r.FormValue("method"))

	target, err := s.App.DB.GetUser(id)
	var notice string
	if err == nil {
		if aerr := checkAdminAccess(r); aerr != nil {
			err = aerr
		} else if !sso.ValidMethod(method) {
			err = errors.New("unknown sign-in method")
		} else if target.ID == actor.ID && method == sso.MethodSSO && !s.hasIdentity(actor.ID) {
			err = errors.New("link a sign-in method to your account first — otherwise you lock yourself out")
		} else if uerr := s.App.DB.SetAuthMethod(target.ID, method); uerr != nil {
			err = uerr
		} else {
			notice = target.Username + " now signs in with " + methodLabel(method) + "."
		}
	}
	label := ""
	if target != nil {
		label = target.Username
	}
	s.App.Activity.Record(actor.Username, "Set sign-in method", label, method, err)

	if isHTMX(r) {
		s.serveUsersFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, notice)
}

// handleUserTOTPClear lets an admin clear another user's two-factor setup
// (e.g. a lost authenticator app). Self-service stays in the two-factor card.
func (s *Server) handleUserTOTPClear(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r)
	if actor == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.PathValue("id"), 0))
	target, err := s.App.DB.GetUser(id)
	var notice string
	if err == nil {
		if aerr := checkAdminAccess(r); aerr != nil {
			err = aerr
		} else if target.ID == actor.ID {
			err = errors.New("use the two-factor card below for your own account")
		} else if !target.TOTPEnabled {
			err = errors.New("two-factor is not on for " + target.Username)
		} else if cerr := s.App.DB.ClearTOTP(target.ID); cerr != nil {
			err = cerr
		} else {
			notice = "Two-factor cleared for " + target.Username + "."
		}
	}
	label := ""
	if target != nil {
		label = target.Username
	}
	s.App.Activity.Record(actor.Username, "Clear two-factor", label, "", err)

	if isHTMX(r) {
		s.serveUsersFragments(w, r, err, notice)
		return
	}
	s.finish(w, r, "/settings", err, notice)
}

// hasIdentity reports whether a user linked any SSO identity.
func (s *Server) hasIdentity(userID int64) bool {
	idents, err := s.App.DB.ListIdentities()
	if err != nil {
		return false
	}
	for _, ident := range idents {
		if ident.UserID == userID {
			return true
		}
	}
	return false
}

func methodLabel(method string) string {
	switch method {
	case sso.MethodPassword:
		return "password"
	case sso.MethodSSO:
		return "single sign-on"
	default:
		return "password or single sign-on"
	}
}
