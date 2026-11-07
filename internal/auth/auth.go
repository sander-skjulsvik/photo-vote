// Package auth handles OIDC login and keeps sessions.
//
// Sessions live only in memory, so a voter's sub is never written to disk.
// There is one replica, and a restart just sends people through a quick login.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	flowTTL  = 10 * time.Minute
	maxFlows = 10_000
)

var (
	// ErrNoAccess means the person logged in but isn't in an allowed group.
	ErrNoAccess = errors.New("no access")
)

// LoginError means a login response couldn't be trusted. Both fields are safe to
// log: Reason is a fixed string, and Detail is a library error about the protocol
// such as a wrong audience. Neither holds the person's identity.
type LoginError struct {
	Reason string
	Detail string
}

func (e *LoginError) Error() string { return "login failed: " + e.Reason }

func rejected(reason string, cause error) error {
	e := &LoginError{Reason: reason}
	if cause != nil {
		e.Detail = cause.Error()
	}
	return e
}

// Config configures the login.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	VoterGroup   string
	OwnerGroup   string
	SessionTTL   time.Duration
	// Now can be replaced in tests. It defaults to time.Now.
	Now func() time.Time
}

// Identity is a logged-in person. It's kept in memory only.
type Identity struct {
	Sub   string
	Voter bool
	Owner bool
}

type session struct {
	identity Identity
	expires  time.Time
}

type flow struct {
	state, nonce, verifier, next string
	expires                      time.Time
}

// Manager runs logins and holds sessions.
type Manager struct {
	cfg    Config
	client *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
	sessions map[string]session
	flows    map[string]flow
}

// NewManager creates a Manager. It doesn't contact the identity provider yet,
// so the app can start, and serve its health check, while that is down.
func NewManager(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{
		cfg:      cfg,
		client:   &http.Client{Timeout: 10 * time.Second},
		sessions: make(map[string]session),
		flows:    make(map[string]flow),
	}
}

// discover fetches the provider's configuration, retrying on the next login if
// it fails.
func (m *Manager) discover() (*oidc.Provider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.provider != nil {
		return m.provider, nil
	}
	// This context is kept by the provider to fetch signing keys later, so it
	// can't be tied to a request. The client's timeout bounds each call.
	ctx := oidc.ClientContext(context.Background(), m.client)
	p, err := oidc.NewProvider(ctx, m.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover identity provider: %w", err)
	}
	m.provider = p
	return p, nil
}

func (m *Manager) oauthConfig(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     m.cfg.ClientID,
		ClientSecret: m.cfg.ClientSecret,
		RedirectURL:  m.cfg.RedirectURL,
		Endpoint:     p.Endpoint(),
		// Only what the app needs: no email and no name.
		Scopes: []string{oidc.ScopeOpenID, "groups"},
	}
}

// Begin starts a login. It returns the URL to send the browser to, and a flow ID
// to put in a cookie, which ties the response to this browser.
func (m *Manager) Begin(next string) (authURL, flowID string, err error) {
	p, err := m.discover()
	if err != nil {
		return "", "", err
	}
	f := flow{
		state:    randomToken(),
		nonce:    randomToken(),
		verifier: oauth2.GenerateVerifier(),
		next:     next,
		expires:  m.cfg.Now().Add(flowTTL),
	}
	flowID = randomToken()

	m.mu.Lock()
	m.pruneLocked()
	if len(m.flows) >= maxFlows {
		// Anyone can start a login, so this has to stay bounded. Dropping an
		// arbitrary flow costs its owner at most one retry.
		for id := range m.flows {
			delete(m.flows, id)
			break
		}
	}
	m.flows[flowID] = f
	m.mu.Unlock()

	authURL = m.oauthConfig(p).AuthCodeURL(f.state,
		oidc.Nonce(f.nonce),
		oauth2.S256ChallengeOption(f.verifier))
	return authURL, flowID, nil
}

// Complete finishes a login. A flow can be used only once, whatever the outcome.
func (m *Manager) Complete(ctx context.Context, flowID, state, code string) (token, next string, err error) {
	m.mu.Lock()
	f, ok := m.flows[flowID]
	delete(m.flows, flowID)
	m.mu.Unlock()
	if !ok || m.cfg.Now().After(f.expires) {
		return "", "", rejected("unknown or expired login attempt", nil)
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(f.state)) != 1 {
		return "", "", rejected("state mismatch", nil)
	}

	p, err := m.discover()
	if err != nil {
		return "", "", err
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, m.client)
	tok, err := m.oauthConfig(p).Exchange(ctx, code, oauth2.VerifierOption(f.verifier))
	if err != nil {
		return "", "", rejected("code exchange failed", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return "", "", rejected("no ID token in the response", nil)
	}
	verifier := p.Verifier(&oidc.Config{ClientID: m.cfg.ClientID, Now: m.cfg.Now})
	idToken, err := verifier.Verify(ctx, raw)
	if err != nil {
		return "", "", rejected("ID token failed verification", err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(f.nonce)) != 1 {
		return "", "", rejected("nonce mismatch", nil)
	}
	var claims struct {
		Groups []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", "", rejected("unreadable claims", nil)
	}
	if idToken.Subject == "" {
		return "", "", rejected("no subject", nil)
	}

	id := Identity{
		Sub:   idToken.Subject,
		Voter: slices.Contains(claims.Groups, m.cfg.VoterGroup),
		Owner: slices.Contains(claims.Groups, m.cfg.OwnerGroup),
	}
	if !id.Voter && !id.Owner {
		return "", "", ErrNoAccess
	}

	token = randomToken()
	m.mu.Lock()
	m.sessions[token] = session{identity: id, expires: m.cfg.Now().Add(m.cfg.SessionTTL)}
	m.mu.Unlock()
	return token, f.next, nil
}

// Session returns the identity for a session token.
func (m *Manager) Session(token string) (Identity, bool) {
	if token == "" {
		return Identity{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[token]
	if !ok {
		return Identity{}, false
	}
	if m.cfg.Now().After(s.expires) {
		delete(m.sessions, token)
		return Identity{}, false
	}
	return s.identity, true
}

// End removes a session.
func (m *Manager) End(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
}

// EndSessionURL returns where to send the browser to log out at the provider,
// or "" if it isn't known.
func (m *Manager) EndSessionURL() string {
	p, err := m.discover()
	if err != nil {
		return ""
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&meta); err != nil {
		return ""
	}
	return meta.EndSession
}

func (m *Manager) pruneLocked() {
	now := m.cfg.Now()
	for id, f := range m.flows {
		if now.After(f.expires) {
			delete(m.flows, id)
		}
	}
	for t, s := range m.sessions {
		if now.After(s.expires) {
			delete(m.sessions, t)
		}
	}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
