package sso

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/models"
)

// Sign-in methods assignable per user. 'either' is the default and preserves
// the pre-SSO behaviour.
const (
	MethodEither   = "either"
	MethodPassword = "password"
	MethodSSO      = "sso"
)

// PasswordAllowed reports whether the username/password form may be used.
func PasswordAllowed(method string) bool {
	return method == "" || method == MethodEither || method == MethodPassword
}

// SSOAllowed reports whether single sign-on may be used.
func SSOAllowed(method string) bool {
	return method == "" || method == MethodEither || method == MethodSSO
}

// ValidMethod reports whether a method value can be assigned.
func ValidMethod(method string) bool {
	return method == MethodEither || method == MethodPassword || method == MethodSSO
}

// ProviderView is the safe subset of a provider for templates: never a secret.
type ProviderView struct {
	ID          int64
	Name        string
	Issuer      string
	ClientID    string
	HasSecret   bool
	ButtonLabel string
	DefaultRole string
	RequireMFA  bool
	Enabled     bool
	// CallbackURL is the exact redirect URI to register at the IdP.
	CallbackURL string
}

// ProviderInput is the validated shape of the Settings provider form. An
// empty ClientSecret on an existing provider keeps the stored secret.
type ProviderInput struct {
	ID           int64
	Name         string
	Issuer       string
	ClientID     string
	ClientSecret string
	ButtonLabel  string
	DefaultRole  string
	RequireMFA   bool
	Enabled      bool
}

// Login is a verified SSO login, ready to be matched to a console user.
type Login struct {
	ProviderName  string
	ProviderID    int64
	Subject       string
	Username      string
	Email         string
	EmailVerified bool
	MFA           bool
	Next          string // post-login target stashed at start, same-site only
}

// idClaims is the subset of ID-token claims the console reads.
type idClaims struct {
	Subject           string   `json:"sub"`
	PreferredUsername string   `json:"preferred_username"`
	Name              string   `json:"name"`
	Email             string   `json:"email"`
	EmailVerified     *bool    `json:"email_verified"`
	AMR               []string `json:"amr"`
}

// mfaMarkers are amr values that prove a second factor was used.
var mfaMarkers = []string{"mfa", "otp", "totp", "hotp", "sms", "push", "voice", "fido", "webauthn", "hwk", "swk"}

// Service owns SSO providers, discovery and the login flow.
type Service struct {
	db     *database.DB
	cfg    *config.Config
	log    *slog.Logger
	states *stateStore

	mu         sync.Mutex
	discovered map[int64]discoveredProvider
}

type discoveredProvider struct {
	issuer   string
	clientID string
	verifier *oidc.IDTokenVerifier
	endpoint oauth2.Endpoint
}

// New creates the SSO service.
func New(db *database.DB, cfg *config.Config, log *slog.Logger) *Service {
	return &Service{
		db:         db,
		cfg:        cfg,
		log:        log,
		states:     newStateStore(),
		discovered: map[int64]discoveredProvider{},
	}
}

// bustDiscovery drops cached issuers after any provider write.
func (s *Service) bustDiscovery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discovered = map[int64]discoveredProvider{}
}

// PublicProviders lists enabled providers for the sign-in page.
func (s *Service) PublicProviders() ([]ProviderView, error) {
	all, err := s.db.ListProviders()
	if err != nil {
		return nil, err
	}

	var out []ProviderView
	for _, p := range all {
		if !p.Enabled {
			continue
		}
		out = append(out, s.viewOf(p))
	}
	return out, nil
}

// ProviderViews lists every provider for the Settings UI.
func (s *Service) ProviderViews() ([]ProviderView, error) {
	all, err := s.db.ListProviders()
	if err != nil {
		return nil, err
	}

	out := make([]ProviderView, 0, len(all))
	for _, p := range all {
		out = append(out, s.viewOf(p))
	}
	return out, nil
}

func (s *Service) viewOf(p models.SSOProvider) ProviderView {
	label := p.ButtonLabel
	if label == "" {
		label = p.Name
	}
	base := strings.TrimSuffix(strings.TrimSpace(s.cfg.BaseURL), "/")
	return ProviderView{
		ID:          p.ID,
		Name:        p.Name,
		Issuer:      p.Issuer,
		ClientID:    p.ClientID,
		HasSecret:   p.ClientSecret != "",
		ButtonLabel: label,
		DefaultRole: p.DefaultRole,
		RequireMFA:  p.RequireMFA,
		Enabled:     p.Enabled,
		CallbackURL: fmt.Sprintf("%s/auth/sso/callback/%d", base, p.ID),
	}
}

// SaveProvider validates, encrypts and stores a provider, checking issuer
// discovery immediately so typos fail fast with a clear message.
func (s *Service) SaveProvider(in ProviderInput) (*models.SSOProvider, error) {
	name := strings.TrimSpace(in.Name)
	if len(name) < 2 || len(name) > 64 {
		return nil, errors.New("provider names must be between 2 and 64 characters")
	}

	issuer := strings.TrimSpace(strings.TrimSuffix(in.Issuer, "/"))
	parsed, err := url.Parse(issuer)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, errors.New("issuer must be a valid https URL")
	}
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return nil, errors.New("plain http issuers are only allowed on localhost for development")
	}

	clientID := strings.TrimSpace(in.ClientID)
	if clientID == "" {
		return nil, errors.New("client ID is required")
	}

	switch in.DefaultRole {
	case auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer:
	default:
		return nil, errors.New("unknown default role")
	}

	var existing *models.SSOProvider
	if in.ID != 0 {
		existing, err = s.db.GetProvider(in.ID)
		if err != nil {
			return nil, err
		}
	}

	// Provider names must stay unique so URLs and buttons are unambiguous.
	all, err := s.db.ListProviders()
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.ID != in.ID && strings.EqualFold(p.Name, name) {
			return nil, errors.New("a provider called " + p.Name + " already exists")
		}
	}

	secret := strings.TrimSpace(in.ClientSecret)
	encrypted := ""
	if secret != "" {
		if !s.cfg.SSOKeySet() {
			return nil, errors.New("set CONSOLE_SSO_KEY and restart before saving a client secret")
		}
		encrypted, err = EncryptSecret(s.cfg.SSOKey, []byte(secret))
		if err != nil {
			return nil, err
		}
	} else if existing != nil {
		encrypted = existing.ClientSecret
	} else {
		return nil, errors.New("client secret is required")
	}

	label := strings.TrimSpace(in.ButtonLabel)
	if label == "" {
		label = name
	}

	row := &models.SSOProvider{
		Name:         name,
		Issuer:       issuer,
		ClientID:     clientID,
		ClientSecret: encrypted,
		ButtonLabel:  label,
		DefaultRole:  in.DefaultRole,
		RequireMFA:   in.RequireMFA,
		Enabled:      in.Enabled,
	}
	if existing != nil {
		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
	}

	// Fail fast: an unreachable or non-OIDC issuer is a form error, not a
	// surprise at the first sign-in.
	if _, err := s.discover(row); err != nil {
		return nil, fmt.Errorf("issuer discovery failed: %w", err)
	}

	if err := s.db.SaveProvider(row); err != nil {
		return nil, err
	}
	s.bustDiscovery()
	return row, nil
}

// DeleteProvider removes a provider. Providers with linked accounts are
// protected: reassign those users to another method first.
func (s *Service) DeleteProvider(id int64) error {
	n, err := s.db.CountIdentitiesForProvider(id)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("cannot delete: %d account%s still linked — set them to password sign-in first", n, plural(n))
	}
	if err := s.db.DeleteProvider(id); err != nil {
		return err
	}
	s.bustDiscovery()
	return nil
}

// SetEnabled toggles a provider without touching anything else.
func (s *Service) SetEnabled(id int64, enabled bool) error {
	p, err := s.db.GetProvider(id)
	if err != nil {
		return err
	}
	p.Enabled = enabled
	if err := s.db.SaveProvider(p); err != nil {
		return err
	}
	s.bustDiscovery()
	return nil
}

// IdentityLabels maps console user ids to their linked provider names for
// display in the users list.
func (s *Service) IdentityLabels() map[int64][]string {
	out := map[int64][]string{}

	providers, err := s.db.ListProviders()
	if err != nil {
		return out
	}
	names := map[int64]string{}
	for _, p := range providers {
		names[p.ID] = p.Name
	}

	idents, err := s.db.ListIdentities()
	if err != nil {
		return out
	}
	for _, ident := range idents {
		name, ok := names[ident.ProviderID]
		if !ok {
			continue
		}
		out[ident.UserID] = append(out[ident.UserID], name)
	}
	return out
}

// AuthURL starts a login: it stores state, nonce and PKCE verifier and
// returns the IdP URL to redirect to.
func (s *Service) AuthURL(providerID int64, next string) (string, error) {
	p, err := s.db.GetProvider(providerID)
	if err != nil {
		return "", err
	}
	if !p.Enabled {
		return "", errors.New("this sign-in method is disabled")
	}

	verifier := oauth2.GenerateVerifier()
	nonce, err := newID()
	if err != nil {
		return "", err
	}
	state, err := s.states.save(providerID, next, nonce, verifier)
	if err != nil {
		return "", err
	}

	cfg, _, err := s.oauthConfig(p)
	if err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	), nil
}

// Callback completes a login: state check, code exchange, token verification
// and claim extraction.
func (s *Service) Callback(ctx context.Context, providerID int64, code, state string) (*Login, error) {
	st, ok := s.states.consume(strings.TrimSpace(state))
	if !ok {
		return nil, errors.New("this sign-in expired or was already used — please try again")
	}
	if st.providerID != providerID {
		return nil, errors.New("sign-in provider mismatch — please try again")
	}

	p, err := s.db.GetProvider(providerID)
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, errors.New("this sign-in method is disabled")
	}

	oauthCfg, disc, err := s.oauthConfig(p)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	token, err := oauthCfg.Exchange(ctx, strings.TrimSpace(code),
		oauth2.VerifierOption(st.verifier))
	if err != nil {
		return nil, errors.New("could not complete the sign-in with the provider")
	}

	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, errors.New("the provider did not return an identity token")
	}
	idToken, err := disc.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, errors.New("could not verify the identity token")
	}
	if idToken.Nonce != st.nonce {
		return nil, errors.New("sign-in verification failed — please try again")
	}

	var claims idClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, errors.New("could not read the identity token")
	}
	if claims.Subject == "" {
		return nil, errors.New("the provider did not identify the account")
	}

	login := &Login{
		ProviderName:  p.Name,
		ProviderID:    p.ID,
		Subject:       claims.Subject,
		Username:      loginName(claims),
		Email:         strings.TrimSpace(claims.Email),
		EmailVerified: claims.EmailVerified != nil && *claims.EmailVerified,
		MFA:           mfaClaimed(claims.AMR),
		Next:          st.next,
	}

	if p.RequireMFA && !login.MFA {
		return nil, errors.New("this provider requires multi-factor sign-in, but the login did not include it")
	}
	return login, nil
}

// discover resolves an issuer to its endpoints and token verifier, cached
// until the next provider write.
func (s *Service) discover(p *models.SSOProvider) (discoveredProvider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cached, ok := s.discovered[p.ID]; ok && cached.issuer == p.Issuer && cached.clientID == p.ClientID {
		return cached, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	provider, err := oidc.NewProvider(ctx, p.Issuer)
	if err != nil {
		return discoveredProvider{}, err
	}

	cached := discoveredProvider{
		issuer:   p.Issuer,
		clientID: p.ClientID,
		verifier: provider.Verifier(&oidc.Config{ClientID: p.ClientID}),
		endpoint: provider.Endpoint(),
	}
	s.discovered[p.ID] = cached
	return cached, nil
}

// oauthConfig builds the OAuth2 client for a provider, decrypting its secret.
func (s *Service) oauthConfig(p *models.SSOProvider) (oauth2.Config, discoveredProvider, error) {
	disc, err := s.discover(p)
	if err != nil {
		return oauth2.Config{}, discoveredProvider{}, err
	}

	secret, err := DecryptSecret(s.cfg.SSOKey, p.ClientSecret)
	if err != nil {
		return oauth2.Config{}, discoveredProvider{},
			errors.New("cannot read the stored client secret — check CONSOLE_SSO_KEY")
	}

	base := strings.TrimSuffix(strings.TrimSpace(s.cfg.BaseURL), "/")
	return oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: string(secret),
		Endpoint:     disc.endpoint,
		RedirectURL:  fmt.Sprintf("%s/auth/sso/callback/%d", base, p.ID),
		Scopes:       []string{"openid", "profile", "email"},
	}, disc, nil
}

// loginName picks a display name from the token claims.
func loginName(claims idClaims) string {
	for _, candidate := range []string{claims.PreferredUsername, claims.Name, claims.Email} {
		if name := strings.TrimSpace(candidate); name != "" {
			return name
		}
	}
	return claims.Subject
}

// mfaClaimed reports whether the token's amr claim proves a second factor.
func mfaClaimed(amr []string) bool {
	for _, method := range amr {
		m := strings.ToLower(strings.TrimSpace(method))
		for _, marker := range mfaMarkers {
			if m == marker {
				return true
			}
		}
	}
	return false
}

// SuggestUsername sanitises a base name to the console's username rules and
// finds a free variant, honouring exists().
func SuggestUsername(exists func(string) bool, base string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(base)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_' || r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '@':
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-._")
	if len(name) < 3 {
		name = "sso-user"
	}
	if len(name) > 28 {
		name = name[:28]
	}
	if !exists(name) {
		return name
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", name, i)
		if len(candidate) > 32 {
			candidate = name[:32-len(fmt.Sprintf("-%d", i))] + fmt.Sprintf("-%d", i)
		}
		if !exists(candidate) {
			return candidate
		}
	}
}

func isLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
