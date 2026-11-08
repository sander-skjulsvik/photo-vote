// Package testidp is a fake OIDC identity provider for tests and the dev server.
// It checks the parts of the protocol the app must get right, including PKCE.
// The real binary doesn't import it.
package testidp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// ClientSecret is the only secret the fake provider accepts. It's not a real one.
const ClientSecret = "fake-client-secret"

// User is a fake account.
type User struct {
	Sub    string
	Label  string
	Groups []string
}

type grant struct {
	user                     User
	nonce, challenge, method string
	redirect, scope          string
}

// IDP is a running fake provider.
type IDP struct {
	URL      string
	ClientID string
	server   *http.Server
	key      *rsa.PrivateKey

	mu     sync.Mutex
	users  []User
	auto   *User
	grants map[string]grant
	mutate func(map[string]any)
}

// Start runs a provider on addr, for example "127.0.0.1:0".
func Start(addr, clientID string, users []User) (*IDP, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	idp := &IDP{
		URL:      "http://" + l.Addr().String(),
		ClientID: clientID,
		key:      key,
		users:    users,
		grants:   make(map[string]grant),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("GET /jwks", idp.jwks)
	mux.HandleFunc("GET /authorize", idp.authorize)
	mux.HandleFunc("POST /token", idp.token)
	mux.HandleFunc("GET /end-session", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "Logged out of the fake provider.")
	})
	idp.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go idp.server.Serve(l)
	return idp, nil
}

// Close stops the provider.
func (i *IDP) Close() { i.server.Close() }

// AutoLogin makes /authorize log in as this user with no picker page.
func (i *IDP) AutoLogin(u User) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.auto = &u
}

// MutateClaims changes the ID token's claims before signing, to simulate a
// misbehaving provider or an attack. nil removes the hook.
func (i *IDP) MutateClaims(fn func(map[string]any)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.mutate = fn
}

func (i *IDP) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                i.URL,
		"authorization_endpoint":                i.URL + "/authorize",
		"token_endpoint":                        i.URL + "/token",
		"jwks_uri":                              i.URL + "/jwks",
		"end_session_endpoint":                  i.URL + "/end-session",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
	})
}

func (i *IDP) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: i.key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig",
	}}})
}

func (i *IDP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != i.ClientID || q.Get("response_type") != "code" {
		http.Error(w, "bad client or response type", http.StatusBadRequest)
		return
	}
	i.mu.Lock()
	auto := i.auto
	i.mu.Unlock()

	user := auto
	if user == nil {
		sub := q.Get("as")
		for n := range i.users {
			if i.users[n].Sub == sub {
				user = &i.users[n]
			}
		}
	}
	if user == nil {
		i.picker(w, r)
		return
	}

	code := token()
	i.mu.Lock()
	i.grants[code] = grant{
		user:      *user,
		nonce:     q.Get("nonce"),
		challenge: q.Get("code_challenge"),
		method:    q.Get("code_challenge_method"),
		redirect:  q.Get("redirect_uri"),
		scope:     q.Get("scope"),
	}
	i.mu.Unlock()

	target, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	v := target.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	target.RawQuery = v.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// picker lets someone choose a fake user on the dev server.
func (i *IDP) picker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><title>Fake login</title><h1>Fake login</h1><p>Log in as:</p><ul>`)
	for _, u := range i.users {
		q := r.URL.Query()
		q.Set("as", u.Sub)
		fmt.Fprintf(w, `<li><a href="/authorize?%s">%s</a></li>`, html.EscapeString(q.Encode()), html.EscapeString(u.Label))
	}
	fmt.Fprint(w, `</ul>`)
}

func (i *IDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != i.ClientID || secret != ClientSecret {
		tokenError(w, "invalid_client")
		return
	}

	code := r.PostForm.Get("code")
	i.mu.Lock()
	g, ok := i.grants[code]
	delete(i.grants, code)
	mutate := i.mutate
	i.mu.Unlock()
	if !ok || r.PostForm.Get("redirect_uri") != g.redirect {
		tokenError(w, "invalid_grant")
		return
	}

	// PKCE: the app has to prove it started this login.
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if g.method != "S256" || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		tokenError(w, "invalid_grant")
		return
	}

	now := time.Now()
	claims := map[string]any{
		"iss":   i.URL,
		"sub":   g.user.Sub,
		"aud":   i.ClientID,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"nonce": g.nonce,
	}
	if slices.Contains(strings.Fields(g.scope), "groups") {
		claims["groups"] = g.user.Groups
	}
	if mutate != nil {
		mutate(claims)
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: i.key},
		(&jose.SignerOptions{}).WithHeader("kid", "test"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	idToken, err := jws.CompactSerialize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"access_token": token(),
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func token() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
