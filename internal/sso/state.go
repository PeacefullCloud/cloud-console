package sso

import (
	"crypto/rand"
	"encoding/base32"
	"sync"
	"time"
)

// loginStateTTL bounds an SSO attempt: the provider must call back quickly.
const loginStateTTL = 10 * time.Minute

// loginState is a pending SSO login: the state token the callback must echo,
// the nonce the ID token must repeat, and the PKCE verifier for the exchange.
type loginState struct {
	providerID int64
	next       string
	nonce      string
	verifier   string
	expires    time.Time
}

// stateStore owns pending SSO logins. Like the TOTP challenges, the console
// is a single process, so guarded in-memory state with expiry is enough.
type stateStore struct {
	mu    sync.Mutex
	items map[string]loginState
}

func newStateStore() *stateStore {
	return &stateStore{items: map[string]loginState{}}
}

// newID returns a random token suitable for state and nonce values.
func newID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// save records a pending login and returns its state token.
func (s *stateStore) save(providerID int64, next, nonce, verifier string) (string, error) {
	state, err := newID()
	if err != nil {
		return "", err
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	for key, item := range s.items {
		if now.After(item.expires) {
			delete(s.items, key)
		}
	}
	s.items[state] = loginState{
		providerID: providerID,
		next:       next,
		nonce:      nonce,
		verifier:   verifier,
		expires:    now.Add(loginStateTTL),
	}
	return state, nil
}

// consume resolves a state token exactly once.
func (s *stateStore) consume(state string) (loginState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	item, ok := s.items[state]
	if !ok {
		return loginState{}, false
	}
	delete(s.items, state)
	if time.Now().After(item.expires) {
		return loginState{}, false
	}
	return item, true
}
