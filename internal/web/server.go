// Package web serves the site.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sander-skjulsvik/photo-vote/internal/auth"
	"github.com/sander-skjulsvik/photo-vote/internal/store"
)

//go:embed templates/*.html static/style.css
var assets embed.FS

const (
	maxFormBytes = 1 << 20

	sessionCookie = "photo_vote_session"
	flowCookie    = "photo_vote_login"
)

// Config configures the server.
type Config struct {
	BaseURL     string
	Issuer      string
	VoterSecret []byte
	Logger      *slog.Logger
}

// Server is the web application.
type Server struct {
	cfg       Config
	store     *store.Store
	auth      *auth.Manager
	log       *slog.Logger
	pages     map[string]*template.Template
	secure    bool
	csp       string
	uploads   chan struct{}
	handler   http.Handler
	routeList []route
}

type access int

const (
	// accessPublic needs no login.
	accessPublic access = iota
	// accessMember needs a voter or the owner.
	accessMember
	// accessVoter needs someone in the voter group.
	accessVoter
	// accessOwner needs the owner.
	accessOwner
)

// route is one entry in the table of everything the app serves. Every route
// states its access level here, and the tests walk this table, so a new route
// can't be forgotten.
type route struct {
	pattern string
	access  access
	handler http.HandlerFunc
	// bodyLimit overrides maxFormBytes. Uploads enforce their own limits.
	bodyLimit int64
}

func (s *Server) routes() []route {
	return []route{
		{pattern: "GET /healthz", access: accessPublic, handler: s.healthz},
		{pattern: "GET /static/style.css", access: accessPublic, handler: s.stylesheet},
		{pattern: "GET /login", access: accessPublic, handler: s.login},
		{pattern: "GET /auth/callback", access: accessPublic, handler: s.callback},
		{pattern: "POST /logout", access: accessPublic, handler: s.logout},
		{pattern: "GET /signed-out", access: accessPublic, handler: s.signedOut},

		{pattern: "GET /{$}", access: accessMember, handler: s.home},
		{pattern: "GET /polls/{id}", access: accessMember, handler: s.pollPage},
		{pattern: "GET /images/{id}", access: accessMember, handler: s.image},
		{pattern: "POST /polls/{id}/ratings", access: accessVoter, handler: s.saveRatings},

		{pattern: "GET /admin", access: accessOwner, handler: s.adminHome},
		{pattern: "POST /admin/polls", access: accessOwner, handler: s.adminCreatePoll},
		{pattern: "GET /admin/polls/{id}", access: accessOwner, handler: s.adminPoll},
		{pattern: "POST /admin/polls/{id}/photos", access: accessOwner, handler: s.adminUpload, bodyLimit: maxUploadBytes},
		{pattern: "POST /admin/polls/{id}/open", access: accessOwner, handler: s.adminSetState(store.StateDraft, store.StateOpen)},
		{pattern: "POST /admin/polls/{id}/close", access: accessOwner, handler: s.adminSetState(store.StateOpen, store.StateClosed)},
		{pattern: "POST /admin/polls/{id}/reopen", access: accessOwner, handler: s.adminSetState(store.StateClosed, store.StateOpen)},
		{pattern: "POST /admin/polls/{id}/delete", access: accessOwner, handler: s.adminDeletePoll},
		{pattern: "POST /admin/items/{id}/description", access: accessOwner, handler: s.adminSetDescription},
		{pattern: "POST /admin/items/{id}/delete", access: accessOwner, handler: s.adminDeleteItem},
	}
}

// New builds the server.
func New(cfg Config, st *store.Store, am *auth.Manager) (*Server, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	issuer, err := url.Parse(cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("parse issuer: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:     cfg,
		store:   st,
		auth:    am,
		log:     cfg.Logger,
		pages:   pages,
		secure:  base.Scheme == "https",
		uploads: make(chan struct{}, 1),
	}
	// No scripts at all. form-action needs the issuer, because browsers also
	// apply it to the redirect after logging out.
	s.csp = "default-src 'none'; img-src 'self'; style-src 'self'; " +
		"form-action 'self' " + issuer.Scheme + "://" + issuer.Host + "; " +
		"base-uri 'none'; frame-ancestors 'none'"

	mux := http.NewServeMux()
	s.routeList = s.routes()
	for _, rt := range s.routeList {
		mux.Handle(rt.pattern, s.guard(rt))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, r, http.StatusNotFound, "That page doesn't exist.")
	})

	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, r, http.StatusForbidden, "This request came from another site, so it was refused.")
	}))
	s.handler = s.recoverPanics(s.securityHeaders(s.logRequests(csrf.Handler(mux))))
	return s, nil
}

// Handler returns the HTTP handler for the whole site.
func (s *Server) Handler() http.Handler { return s.handler }

type identityKey struct{}

func identityFrom(r *http.Request) auth.Identity {
	id, _ := r.Context().Value(identityKey{}).(auth.Identity)
	return id
}

// guard enforces a route's access level before its handler runs.
func (s *Server) guard(rt route) http.Handler {
	limit := rt.bodyLimit
	if limit == 0 {
		limit = maxFormBytes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if rt.access == accessPublic {
			rt.handler(w, r)
			return
		}

		id, ok := s.auth.Session(s.cookieValue(r, sessionCookie))
		if !ok {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.Path), http.StatusFound)
				return
			}
			s.renderError(w, r, http.StatusUnauthorized, "Your login has expired. Go back and log in again.")
			return
		}

		allowed := false
		switch rt.access {
		case accessMember:
			allowed = id.Voter || id.Owner
		case accessVoter:
			allowed = id.Voter
		case accessOwner:
			allowed = id.Owner
		}
		if !allowed {
			s.renderError(w, r, http.StatusForbidden, "You don't have access to this.")
			return
		}
		rt.handler(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", s.csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		// Pages hold personal data. Handlers for images and the stylesheet
		// replace this with their own value.
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests logs the route pattern rather than the path, along with the status
// and duration. Paths contain poll and item IDs, which could help tie a person's
// activity together, so they stay out of the logs. So do client addresses.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.Pattern == "GET /healthz" {
			return
		}
		pattern := r.Pattern
		if pattern == "" {
			pattern = "(no route)"
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		s.log.Info("request", "route", pattern, "status", status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			// The panic value could hold request data, so log only the stack.
			s.log.Error("panic while handling a request", "stack", string(debug.Stack()))
			http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// cookieName uses the __Host- prefix on HTTPS, which makes browsers refuse a
// cookie set by a subdomain or without Secure.
func (s *Server) cookieName(name string) string {
	if s.secure {
		return "__Host-" + name
	}
	return name
}

func (s *Server) cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(s.cookieName(name))
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(name),
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		Secure:   s.secure,
		HttpOnly: true,
		// Lax, not Strict: the login returns from the identity provider through
		// a top-level redirect, which has to carry the cookie. POSTs from other
		// sites don't send it.
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(name),
		Path:     "/",
		MaxAge:   -1,
		Secure:   s.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// VoterHash is what ratings are stored under. It's keyed so a database copy
// alone reveals nothing, and includes the poll so one person can't be followed
// across polls. Each field is length-prefixed so two different inputs can never
// produce the same message.
func VoterHash(secret []byte, pollID, sub string) []byte {
	mac := hmac.New(sha256.New, secret)
	for _, field := range []string{pollID, sub} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		mac.Write(size[:])
		mac.Write([]byte(field))
	}
	return mac.Sum(nil)
}

// safeNext returns next only if it's a path on this site, to prevent the login
// from being used to redirect people elsewhere.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return next
}

// fail turns a store error into a response. Unexpected errors are logged by
// kind only, as they could quote data from the query.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "That page doesn't exist.")
	case errors.Is(err, store.ErrConflict):
		s.renderError(w, r, http.StatusConflict, "That can't be changed now, because the poll has moved on.")
	case errors.Is(err, store.ErrLimit):
		s.renderError(w, r, http.StatusRequestEntityTooLarge, "The storage or photo limit has been reached.")
	default:
		s.log.Error("request failed", "route", r.Pattern, "error_type", fmt.Sprintf("%T", err))
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong.")
	}
}
