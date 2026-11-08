package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sander-skjulsvik/photo-vote/internal/auth"
	"github.com/sander-skjulsvik/photo-vote/internal/imaging"
	"github.com/sander-skjulsvik/photo-vote/internal/store"
	"github.com/sander-skjulsvik/photo-vote/internal/testidp"
	"github.com/sander-skjulsvik/photo-vote/internal/testimg"
)

const (
	voterGroup = "test-voters"
	ownerGroup = "test-owners"
	clientID   = "photo-vote-test"
)

var (
	owner    = testidp.User{Sub: "SUB-OWNER-1111", Label: "Owner", Groups: []string{ownerGroup}}
	voterA   = testidp.User{Sub: "SUB-VOTER-AAAA", Label: "Voter A", Groups: []string{voterGroup}}
	voterB   = testidp.User{Sub: "SUB-VOTER-BBBB", Label: "Voter B", Groups: []string{voterGroup}}
	outsider = testidp.User{Sub: "SUB-OUTSIDER-99", Label: "Outsider", Groups: []string{"somewhere-else"}}
	noGroups = testidp.User{Sub: "SUB-NO-GROUPS-0", Label: "No groups"}
)

var bg = context.Background()

type env struct {
	t      *testing.T
	base   string
	app    *Server
	st     *store.Store
	idp    *testidp.IDP
	logs   *bytes.Buffer
	dbPath string
	secret []byte
	clock  *atomic.Int64 // seconds added to the current time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	idp, err := testidp.Start("127.0.0.1:0", clientID, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idp.Close)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath, store.Limits{MaxItemsPerPoll: 20, MaxStorageBytes: 50 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ts := httptest.NewUnstartedServer(nil)
	base := "http://" + ts.Listener.Addr().String()
	clock := &atomic.Int64{}
	am := auth.NewManager(auth.Config{
		Issuer:       idp.URL,
		ClientID:     clientID,
		ClientSecret: testidp.ClientSecret,
		RedirectURL:  base + "/auth/callback",
		VoterGroup:   voterGroup,
		OwnerGroup:   ownerGroup,
		SessionTTL:   SessionTTL,
		Now:          func() time.Time { return time.Now().Add(time.Duration(clock.Load()) * time.Second) },
	})
	logs := &bytes.Buffer{}
	secret := []byte(strings.Repeat("test-only", 4))
	app, err := New(Config{
		BaseURL:     base,
		Issuer:      idp.URL,
		VoterSecret: secret,
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, st, am)
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = app.Handler()
	ts.Start()
	t.Cleanup(ts.Close)

	return &env{t: t, base: base, app: app, st: st, idp: idp, logs: logs, dbPath: dbPath, secret: secret, clock: clock}
}

type response struct {
	Status   int
	Body     string
	Header   http.Header
	Location string
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

// request sends a request and doesn't follow redirects.
func (e *env) request(c *http.Client, method, path string, body io.Reader, contentType string, headers map[string]string) response {
	e.t.Helper()
	target := path
	if strings.HasPrefix(path, "/") {
		target = e.base + path
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	noFollow := *c
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := noFollow.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return response{Status: res.StatusCode, Body: string(data), Header: res.Header, Location: res.Header.Get("Location")}
}

func (e *env) get(c *http.Client, path string) response {
	e.t.Helper()
	return e.request(c, http.MethodGet, path, nil, "", nil)
}

func (e *env) post(c *http.Client, path string, form url.Values) response {
	e.t.Helper()
	return e.request(c, http.MethodPost, path, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil)
}

// login goes through the whole OIDC flow and returns a logged-in client.
func (e *env) login(u testidp.User) *http.Client {
	e.t.Helper()
	c := newClient()
	e.idp.AutoLogin(u)
	res, err := c.Get(e.base + "/login")
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("login as %s ended with status %d", u.Label, res.StatusCode)
	}
	return c
}

// startLogin runs a login up to the point where the browser would come back to
// the app, and returns that callback URL without visiting it.
func (e *env) startLogin(c *http.Client, u testidp.User) string {
	e.t.Helper()
	e.idp.AutoLogin(u)
	res := e.get(c, "/login")
	if res.Status != http.StatusFound {
		e.t.Fatalf("/login status %d", res.Status)
	}
	res = e.get(c, res.Location)
	if res.Status != http.StatusFound || !strings.HasPrefix(res.Location, e.base+"/auth/callback") {
		e.t.Fatalf("provider didn't redirect back: %d %q", res.Status, res.Location)
	}
	return res.Location
}

func (e *env) loggedIn(c *http.Client) bool {
	e.t.Helper()
	return e.get(c, "/").Status == http.StatusOK
}

func photo(t *testing.T, seed int) store.Photo {
	t.Helper()
	res, err := imaging.Process(testimg.JPEG(testimg.Gradient(60, 40, seed)))
	if err != nil {
		t.Fatal(err)
	}
	return store.Photo{JPEG: res.JPEG, Width: res.Width, Height: res.Height}
}

// poll creates a poll directly in the database, in the given state.
func (e *env) poll(title string, state store.State, photos int) (store.Poll, []store.Item) {
	e.t.Helper()
	p, err := e.st.CreatePoll(bg, title)
	if err != nil {
		e.t.Fatal(err)
	}
	items := make([]store.Item, photos)
	for i := range items {
		if items[i], err = e.st.AddPhoto(bg, p.ID, photo(e.t, i)); err != nil {
			e.t.Fatal(err)
		}
	}
	if state != store.StateDraft {
		if err := e.st.SetState(bg, p.ID, store.StateDraft, store.StateOpen); err != nil {
			e.t.Fatal(err)
		}
	}
	if state == store.StateClosed {
		if err := e.st.SetState(bg, p.ID, store.StateOpen, store.StateClosed); err != nil {
			e.t.Fatal(err)
		}
	}
	p.State = state
	return p, items
}

func rate(items []store.Item, scores ...int) url.Values {
	form := url.Values{}
	for i, score := range scores {
		form.Set("rating_"+items[i].ID, fmt.Sprint(score))
	}
	return form
}

type file struct {
	name string
	data []byte
}

func (e *env) upload(c *http.Client, pollID string, files ...file) response {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range files {
		part, err := w.CreateFormFile("photos", f.name)
		if err != nil {
			e.t.Fatal(err)
		}
		part.Write(f.data)
	}
	w.Close()
	return e.request(c, http.MethodPost, "/admin/polls/"+pollID+"/photos", &buf, w.FormDataContentType(), nil)
}

func wantStatus(t *testing.T, r response, want int) {
	t.Helper()
	if r.Status != want {
		t.Errorf("status = %d, want %d. Body: %.200s", r.Status, want, r.Body)
	}
}

// fill replaces the {id} in a route pattern with a real ID for that route.
func fill(pattern, pollID, itemID, photoID string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = strings.ReplaceAll(path, "{$}", "")
	switch {
	case strings.Contains(path, "/items/"):
		path = strings.Replace(path, "{id}", itemID, 1)
	case strings.Contains(path, "/images/"):
		path = strings.Replace(path, "{id}", photoID, 1)
	default:
		path = strings.Replace(path, "{id}", pollID, 1)
	}
	return method, path
}

// --- Access control: these walk the route table, so new routes are covered. ---

func TestOnlyExpectedRoutesArePublic(t *testing.T) {
	e := newEnv(t)
	want := map[string]bool{
		"GET /healthz": true, "GET /static/style.css": true, "GET /login": true,
		"GET /auth/callback": true, "POST /logout": true, "GET /signed-out": true,
	}
	for _, rt := range e.app.routeList {
		if rt.access == accessPublic && !want[rt.pattern] {
			t.Errorf("%s is public, and isn't on the list of routes allowed to be", rt.pattern)
		}
	}
}

func TestAdminRoutesAreOwnerOnly(t *testing.T) {
	e := newEnv(t)
	seen := 0
	for _, rt := range e.app.routeList {
		if strings.Contains(rt.pattern, "/admin") {
			seen++
			if rt.access != accessOwner {
				t.Errorf("%s isn't owner-only", rt.pattern)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no admin routes found")
	}
}

func TestNoAccessWithoutLogin(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Secret poll title", store.StateClosed, 1)
	anon := newClient()
	for _, rt := range e.app.routeList {
		if rt.access == accessPublic {
			continue
		}
		method, path := fill(rt.pattern, p.ID, items[0].ID, items[0].PhotoID)
		r := e.request(anon, method, path, nil, "application/x-www-form-urlencoded", nil)
		if method == http.MethodGet {
			if r.Status != http.StatusFound || !strings.HasPrefix(r.Location, "/login") {
				t.Errorf("%s: %d to %q, want a redirect to /login", rt.pattern, r.Status, r.Location)
			}
		} else if r.Status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", rt.pattern, r.Status)
		}
		if strings.Contains(r.Body, "Secret poll title") {
			t.Errorf("%s: leaked the poll title", rt.pattern)
		}
	}
}

func TestVoterCanNotUseOwnerRoutes(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Draft title only the owner sees", store.StateDraft, 1)
	voter := e.login(voterA)
	for _, rt := range e.app.routeList {
		if rt.access != accessOwner {
			continue
		}
		method, path := fill(rt.pattern, p.ID, items[0].ID, items[0].PhotoID)
		r := e.request(voter, method, path, strings.NewReader("confirm=yes&title=x&description=x"), "application/x-www-form-urlencoded", nil)
		wantStatus(t, r, http.StatusForbidden)
		if strings.Contains(r.Body, "Draft title") {
			t.Errorf("%s: leaked the draft title", rt.pattern)
		}
	}
	got, err := e.st.Poll(bg, p.ID)
	if err != nil || got.State != store.StateDraft {
		t.Errorf("a voter changed the poll: %+v, %v", got, err)
	}
}

func TestOwnerWhoIsNotAVoterCanNotRate(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 1)
	wantStatus(t, e.post(e.login(owner), "/polls/"+p.ID+"/ratings", rate(items, 5)), http.StatusForbidden)
	totals, _, _ := e.st.Totals(bg, p.ID)
	if len(totals) != 0 {
		t.Errorf("the owner's rating was stored: %v", totals)
	}
}

func TestPeopleOutsideTheGroupsGetNoSession(t *testing.T) {
	for _, u := range []testidp.User{outsider, noGroups} {
		t.Run(u.Label, func(t *testing.T) {
			e := newEnv(t)
			p, items := e.poll("Secret poll title", store.StateOpen, 1)
			c := newClient()
			wantStatus(t, e.get(c, e.startLogin(c, u)), http.StatusForbidden)
			if e.loggedIn(c) {
				t.Fatal("got a session")
			}
			for _, path := range []string{"/polls/" + p.ID, "/images/" + items[0].PhotoID, "/admin"} {
				r := e.get(c, path)
				if r.Status != http.StatusFound {
					t.Errorf("%s: status %d, want a redirect to login", path, r.Status)
				}
			}
		})
	}
}

func TestGroupsMustComeFromTheToken(t *testing.T) {
	// Being in the group at the provider is what counts, so a token with no
	// groups claim at all must not get in.
	e := newEnv(t)
	e.idp.MutateClaims(func(c map[string]any) { delete(c, "groups") })
	c := newClient()
	wantStatus(t, e.get(c, e.startLogin(c, voterA)), http.StatusForbidden)
	if e.loggedIn(c) {
		t.Error("got a session with no groups claim")
	}
}

func TestDraftLooksTheSameAsMissing(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Draft", store.StateDraft, 1)
	voter := e.login(voterA)

	draft := e.get(voter, "/polls/"+p.ID)
	missing := e.get(voter, "/polls/doesnotexist")
	wantStatus(t, draft, http.StatusNotFound)
	if draft.Body != missing.Body {
		t.Error("a draft poll can be told apart from a missing one")
	}
	wantStatus(t, e.get(voter, "/images/"+items[0].PhotoID), http.StatusNotFound)
	wantStatus(t, e.post(voter, "/polls/"+p.ID+"/ratings", rate(items, 3)), http.StatusNotFound)
	if strings.Contains(e.get(voter, "/").Body, "Draft") {
		t.Error("a draft poll is listed for voters")
	}
}

// --- Login ---

func TestLoginRequestIsSafeAndMinimal(t *testing.T) {
	e := newEnv(t)
	r := e.get(newClient(), "/login")
	u, err := url.Parse(r.Location)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Error("login doesn't use PKCE with S256")
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Error("login is missing state or nonce")
	}
	if q.Get("scope") != "openid groups" {
		t.Errorf("scope = %q, want only what the app needs", q.Get("scope"))
	}
}

func TestLoginRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong nonce", func(c map[string]any) { c["nonce"] = "something-else" }},
		{"no nonce", func(c map[string]any) { delete(c, "nonce") }},
		{"wrong audience", func(c map[string]any) { c["aud"] = "another-app" }},
		{"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example.com" }},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		{"no subject", func(c map[string]any) { delete(c, "sub") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.idp.MutateClaims(tt.mutate)
			c := newClient()
			wantStatus(t, e.get(c, e.startLogin(c, voterA)), http.StatusBadRequest)
			if e.loggedIn(c) {
				t.Error("got a session")
			}
		})
	}
}

func TestRejectedLoginsLogAReasonButNoIdentity(t *testing.T) {
	e := newEnv(t)
	e.idp.MutateClaims(func(c map[string]any) { c["aud"] = "another-app" })
	c := newClient()
	wantStatus(t, e.get(c, e.startLogin(c, voterA)), http.StatusBadRequest)
	logs := e.logs.String()
	if !strings.Contains(logs, "ID token failed verification") {
		t.Errorf("the log doesn't say why the login failed:\n%s", logs)
	}
	if strings.Contains(logs, voterA.Sub) {
		t.Errorf("the log contains who was logging in:\n%s", logs)
	}
}

func TestCallbackRejectsTamperedOrStolenResponses(t *testing.T) {
	e := newEnv(t)

	t.Run("state changed", func(t *testing.T) {
		c := newClient()
		u, _ := url.Parse(e.startLogin(c, voterA))
		q := u.Query()
		q.Set("state", "forged")
		u.RawQuery = q.Encode()
		wantStatus(t, e.get(c, u.String()), http.StatusBadRequest)
		if e.loggedIn(c) {
			t.Error("got a session")
		}
	})

	t.Run("another browser uses the link", func(t *testing.T) {
		// An attacker tricks someone into finishing the attacker's login.
		link := e.startLogin(newClient(), voterA)
		victim := newClient()
		wantStatus(t, e.get(victim, link), http.StatusBadRequest)
		if e.loggedIn(victim) {
			t.Error("the other browser got a session")
		}
	})

	t.Run("response replayed", func(t *testing.T) {
		c := newClient()
		link := e.startLogin(c, voterA)
		wantStatus(t, e.get(c, link), http.StatusFound)
		fresh := newClient()
		wantStatus(t, e.get(fresh, link), http.StatusBadRequest)
		if e.loggedIn(fresh) {
			t.Error("replay got a session")
		}
	})

	t.Run("provider reports an error", func(t *testing.T) {
		wantStatus(t, e.get(newClient(), "/auth/callback?error=access_denied&state=x"), http.StatusForbidden)
	})
}

func TestLoginReturnsToTheRequestedPage(t *testing.T) {
	e := newEnv(t)
	p, _ := e.poll("Open", store.StateOpen, 1)
	c := newClient()
	first := e.get(c, "/polls/"+p.ID)
	if first.Status != http.StatusFound {
		t.Fatalf("status %d", first.Status)
	}
	e.idp.AutoLogin(voterA)
	r := e.get(c, first.Location)
	r = e.get(c, r.Location)
	r = e.get(c, r.Location)
	wantStatus(t, r, http.StatusFound)
	if r.Location != "/polls/"+p.ID {
		t.Errorf("went to %q after login", r.Location)
	}
}

func TestSafeNext(t *testing.T) {
	tests := map[string]string{
		"/polls/abc":              "/polls/abc",
		"/":                       "/",
		"":                        "/",
		"//evil.example.com":      "/",
		"/\\evil.example.com":     "/",
		"https://evil.example":    "/",
		"javascript:alert(1)":     "/",
		"/ok\r\nSet-Cookie: x=1":  "/",
		"\\\\evil.example.com":    "/",
		"evil.example.com":        "/",
		"/\tevil":                 "/",
		"https:evil.example.com":  "/",
		"/%2f%2fevil.example.com": "/%2f%2fevil.example.com", // stays on this site
	}
	for in, want := range tests {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginDoesNotRedirectOffSite(t *testing.T) {
	e := newEnv(t)
	c := e.login(voterA)
	r := e.get(c, "/login?next="+url.QueryEscape("//evil.example.com/steal"))
	if r.Location != "/" {
		t.Errorf("redirected to %q", r.Location)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	e := newEnv(t)
	c := e.login(voterA)
	if !e.loggedIn(c) {
		t.Fatal("not logged in")
	}
	// Keep the old cookie, as an attacker who had copied it would.
	base, _ := url.Parse(e.base)
	old := c.Jar.Cookies(base)
	r := e.post(c, "/logout", nil)
	wantStatus(t, r, http.StatusSeeOther)
	if r.Location != e.idp.URL+"/end-session" {
		t.Errorf("logout went to %q, want the provider's logout", r.Location)
	}
	if e.loggedIn(c) {
		t.Error("still logged in after logout")
	}
	stolen := newClient()
	stolen.Jar.SetCookies(base, old)
	if e.loggedIn(stolen) {
		t.Error("the old cookie still works after logout")
	}
}

func TestSessionExpires(t *testing.T) {
	e := newEnv(t)
	c := e.login(voterA)
	e.clock.Store(int64(SessionTTL/time.Second) - 60)
	if !e.loggedIn(c) {
		t.Error("session ended early")
	}
	e.clock.Store(int64(SessionTTL/time.Second) + 60)
	if e.loggedIn(c) {
		t.Error("session outlived its limit")
	}
}

func TestCookiesAreLockedDownOnHTTPS(t *testing.T) {
	e := newEnv(t)
	s, err := New(Config{BaseURL: "https://vote.example.com", Issuer: "https://auth.example.com/"}, e.st, e.app.auth)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.setCookie(rec, sessionCookie, "value", time.Hour)
	got := rec.Result().Cookies()[0]
	if !strings.HasPrefix(got.Name, "__Host-") || !got.Secure || !got.HttpOnly || got.SameSite != http.SameSiteLaxMode || got.Path != "/" || got.Domain != "" {
		t.Errorf("cookie isn't locked down: %+v", got)
	}
}

// --- Rating ---

func TestRatingAndChangingIt(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 3)
	voter := e.login(voterA)

	r := e.post(voter, "/polls/"+p.ID+"/ratings", rate(items[:2], 4, 2))
	wantStatus(t, r, http.StatusSeeOther)
	page := e.get(voter, r.Location)
	if !strings.Contains(page.Body, "Your ratings are saved") {
		t.Error("no confirmation after saving")
	}
	if !strings.Contains(page.Body, `<span class="saved-count">2</span></span> of 3 rated`) {
		t.Error("progress doesn't say 2 of 3")
	}
	checked := fmt.Sprintf(`id="r-%s-4" name="rating_%s" value="4" checked`, items[0].ID, items[0].ID)
	if !strings.Contains(page.Body, checked) {
		t.Error("the saved rating isn't selected on the page")
	}

	// Changing a rating replaces it rather than adding another.
	wantStatus(t, e.post(voter, "/polls/"+p.ID+"/ratings", rate(items[:1], 1)), http.StatusSeeOther)
	totals, voters, err := e.st.Totals(bg, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if voters != 1 || totals[items[0].ID].Count != 1 || totals[items[0].ID].Average != 1 {
		t.Errorf("after changing: voters %d, total %+v", voters, totals[items[0].ID])
	}
	if totals[items[1].ID].Average != 2 {
		t.Errorf("a rating not in the form changed: %+v", totals[items[1].ID])
	}

	// No rating clears it.
	wantStatus(t, e.post(voter, "/polls/"+p.ID+"/ratings", rate(items[:1], 0)), http.StatusSeeOther)
	if !strings.Contains(e.get(voter, "/polls/"+p.ID).Body, `<span class="saved-count">1</span></span> of 3 rated`) {
		t.Error("clearing a rating didn't update the progress")
	}
}

func TestInvalidRatingsAreRefused(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 1)
	voter := e.login(voterA)
	for _, bad := range []string{"6", "-1", "abc", "1.5", "", "99999999999999999999"} {
		form := url.Values{"rating_" + items[0].ID: {bad}}
		wantStatus(t, e.post(voter, "/polls/"+p.ID+"/ratings", form), http.StatusBadRequest)
	}
	totals, _, _ := e.st.Totals(bg, p.ID)
	if len(totals) != 0 {
		t.Errorf("an invalid rating was stored: %v", totals)
	}
}

func TestRatingAnItemFromAnotherPollDoesNothing(t *testing.T) {
	e := newEnv(t)
	p, _ := e.poll("Open", store.StateOpen, 1)
	other, otherItems := e.poll("Other", store.StateOpen, 1)
	wantStatus(t, e.post(e.login(voterA), "/polls/"+p.ID+"/ratings", rate(otherItems, 5)), http.StatusSeeOther)
	totals, _, _ := e.st.Totals(bg, other.ID)
	if len(totals) != 0 {
		t.Errorf("a rating reached another poll: %v", totals)
	}
}

func TestNoRatingOnceClosed(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 1)
	voter := e.login(voterA)
	wantStatus(t, e.post(voter, "/polls/"+p.ID+"/ratings", rate(items, 3)), http.StatusSeeOther)
	wantStatus(t, e.post(e.login(owner), "/admin/polls/"+p.ID+"/close", nil), http.StatusSeeOther)
	wantStatus(t, e.post(voter, "/polls/"+p.ID+"/ratings", rate(items, 5)), http.StatusConflict)
	totals, _, _ := e.st.Totals(bg, p.ID)
	if totals[items[0].ID].Average != 3 {
		t.Errorf("rating changed after close: %+v", totals[items[0].ID])
	}
}

func TestDescriptionsAreShownAndEscaped(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("<script>alert('title')</script>", store.StateDraft, 1)
	o := e.login(owner)
	form := url.Values{"description": {"<script>alert('desc')</script> Warm & friendly"}}
	wantStatus(t, e.post(o, "/admin/items/"+items[0].ID+"/description", form), http.StatusSeeOther)
	wantStatus(t, e.post(o, "/admin/polls/"+p.ID+"/open", nil), http.StatusSeeOther)

	page := e.get(e.login(voterA), "/polls/"+p.ID).Body
	if !strings.Contains(page, "Warm &amp; friendly") {
		t.Error("the description isn't on the voter's page")
	}
	if strings.Contains(page, "<script") {
		t.Error("a script tag got through unescaped")
	}
}

func TestDescriptionAndTitleLimits(t *testing.T) {
	e := newEnv(t)
	_, items := e.poll("Draft", store.StateDraft, 1)
	o := e.login(owner)
	long := url.Values{"description": {strings.Repeat("é", maxDescriptionRunes+1)}}
	wantStatus(t, e.post(o, "/admin/items/"+items[0].ID+"/description", long), http.StatusBadRequest)
	fits := url.Values{"description": {strings.Repeat("é", maxDescriptionRunes)}}
	wantStatus(t, e.post(o, "/admin/items/"+items[0].ID+"/description", fits), http.StatusSeeOther)
	wantStatus(t, e.post(o, "/admin/polls", url.Values{"title": {"  "}}), http.StatusBadRequest)
	wantStatus(t, e.post(o, "/admin/polls", url.Values{"title": {strings.Repeat("x", maxTitleRunes+1)}}), http.StatusBadRequest)
}

// --- Who can see results ---

func TestVotersSeeNoTotalsUntilClose(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 2)
	a, b := e.login(voterA), e.login(voterB)
	wantStatus(t, e.post(a, "/polls/"+p.ID+"/ratings", rate(items, 5, 1)), http.StatusSeeOther)

	page := e.get(b, "/polls/"+p.ID).Body
	for _, leak := range []string{"average from", "★</strong>", "1 rating"} {
		if strings.Contains(page, leak) {
			t.Errorf("an open poll shows %q to a voter", leak)
		}
	}
	if strings.Contains(page, `value="5" checked`) || strings.Contains(page, `value="1" checked`) {
		t.Error("voter B's page shows voter A's rating")
	}
	if !strings.Contains(page, `<span class="saved-count">0</span></span> of 2 rated`) {
		t.Error("voter B's progress counts voter A's ratings")
	}
}

func TestOwnerSeesLiveTotalsAndVotersAfterClose(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 2)
	a, b, o := e.login(voterA), e.login(voterB), e.login(owner)
	wantStatus(t, e.post(a, "/polls/"+p.ID+"/ratings", rate(items, 5, 2)), http.StatusSeeOther)
	wantStatus(t, e.post(b, "/polls/"+p.ID+"/ratings", rate(items[:1], 2)), http.StatusSeeOther)

	live := e.get(o, "/admin/polls/"+p.ID).Body
	if !strings.Contains(live, "3.5 ★") || !strings.Contains(live, "2 ratings") || !strings.Contains(live, "2 people") {
		t.Errorf("the owner's live page doesn't show the totals: %.400s", live)
	}

	wantStatus(t, e.post(o, "/admin/polls/"+p.ID+"/close", nil), http.StatusSeeOther)
	results := e.get(b, "/polls/"+p.ID).Body
	if !strings.Contains(results, "3.5 ★") || !strings.Contains(results, "2.0 ★") {
		t.Errorf("voters don't see the results after close: %.400s", results)
	}
	if !strings.Contains(results, "You gave it 2 ★") {
		t.Error("a voter's own rating isn't shown to them")
	}
	if strings.Contains(results, "You gave it 5 ★") {
		t.Error("voter B's page shows voter A's rating")
	}
	if strings.Index(results, "3.5 ★") > strings.Index(results, "2.0 ★") {
		t.Error("results aren't sorted with the highest first")
	}
}

func TestReopeningAllowsRatingAgain(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Closed", store.StateClosed, 1)
	wantStatus(t, e.post(e.login(owner), "/admin/polls/"+p.ID+"/reopen", nil), http.StatusSeeOther)
	wantStatus(t, e.post(e.login(voterA), "/polls/"+p.ID+"/ratings", rate(items, 3)), http.StatusSeeOther)
}

// --- Nothing ties a person to a rating ---

func TestNothingLinksPeopleToRatings(t *testing.T) {
	e := newEnv(t)
	const (
		title       = "A-PRIVATE-TITLE"
		description = "A-PRIVATE-DESCRIPTION"
		filename    = "A-PRIVATE-FILENAME.jpg"
	)
	o, a, b := e.login(owner), e.login(voterA), e.login(voterB)

	res := e.post(o, "/admin/polls", url.Values{"title": {title}})
	wantStatus(t, res, http.StatusSeeOther)
	pollID := strings.TrimPrefix(res.Location, "/admin/polls/")
	wantStatus(t, e.upload(o, pollID, file{filename, testimg.JPEG(testimg.Gradient(60, 40, 1))}), http.StatusSeeOther)
	items, err := e.st.Items(bg, pollID)
	if err != nil || len(items) != 1 {
		t.Fatalf("items %v, err %v", items, err)
	}
	wantStatus(t, e.post(o, "/admin/items/"+items[0].ID+"/description", url.Values{"description": {description}}), http.StatusSeeOther)
	wantStatus(t, e.post(o, "/admin/polls/"+pollID+"/open", nil), http.StatusSeeOther)
	wantStatus(t, e.post(a, "/polls/"+pollID+"/ratings", rate(items, 5)), http.StatusSeeOther)
	wantStatus(t, e.post(b, "/polls/"+pollID+"/ratings", rate(items, 1)), http.StatusSeeOther)
	live := e.get(o, "/admin/polls/"+pollID).Body
	wantStatus(t, e.post(o, "/admin/polls/"+pollID+"/close", nil), http.StatusSeeOther)

	hashA := VoterHash(e.secret, pollID, voterA.Sub)
	hashB := VoterHash(e.secret, pollID, voterB.Sub)
	subs := []string{owner.Sub, voterA.Sub, voterB.Sub}
	identifying := append(append([]string{}, subs...), hex.EncodeToString(hashA), hex.EncodeToString(hashB))

	t.Run("pages", func(t *testing.T) {
		pages := map[string]string{
			"owner live":    live,
			"owner closed":  e.get(o, "/admin/polls/"+pollID).Body,
			"owner list":    e.get(o, "/admin").Body,
			"voter results": e.get(a, "/polls/"+pollID).Body,
			"voter home":    e.get(a, "/").Body,
		}
		for name, page := range pages {
			for _, s := range identifying {
				if strings.Contains(page, s) {
					t.Errorf("%s page contains %q", name, s)
				}
			}
			// Only an average and a count: no breakdown by score, even for the owner.
			for _, breakdown := range []string{"5 ★", "1 ★"} {
				if strings.Contains(page, breakdown) && name != "voter results" {
					t.Errorf("%s page shows an individual score: %q", name, breakdown)
				}
			}
		}
	})

	t.Run("logs", func(t *testing.T) {
		logs := e.logs.String()
		if !strings.Contains(logs, "POST /polls/{id}/ratings") {
			t.Fatalf("test is broken: rating requests weren't logged:\n%s", logs)
		}
		forbidden := append(append([]string{}, identifying...),
			pollID, items[0].ID, items[0].PhotoID, title, description, filename, "score", "127.0.0.1")
		for _, s := range forbidden {
			if strings.Contains(logs, s) {
				t.Errorf("logs contain %q", s)
			}
		}
	})

	t.Run("database", func(t *testing.T) {
		var disk []byte
		for _, suffix := range []string{"", "-wal"} {
			data, err := os.ReadFile(e.dbPath + suffix)
			if err == nil {
				disk = append(disk, data...)
			}
		}
		if !bytes.Contains(disk, []byte(title)) {
			t.Fatal("test is broken: the poll isn't in the files being searched")
		}
		for _, sub := range subs {
			if bytes.Contains(disk, []byte(sub)) {
				t.Errorf("%q is written to disk", sub)
			}
		}
		if bytes.Contains(disk, []byte(filename)) {
			t.Error("the original filename is written to disk")
		}
	})
}

func TestVoterHash(t *testing.T) {
	secret := []byte("secret")
	base := VoterHash(secret, "poll", "sub")
	if !bytes.Equal(base, VoterHash(secret, "poll", "sub")) {
		t.Error("the same voter in the same poll got a different hash, so they couldn't change their rating")
	}
	if bytes.Equal(base, VoterHash(secret, "poll2", "sub")) {
		t.Error("the same voter gets the same hash across polls")
	}
	if bytes.Equal(base, VoterHash(secret, "poll", "sub2")) {
		t.Error("two voters got the same hash")
	}
	if bytes.Equal(base, VoterHash([]byte("other"), "poll", "sub")) {
		t.Error("the hash doesn't depend on the secret")
	}
	if bytes.Equal(VoterHash(secret, "ab", "c"), VoterHash(secret, "a", "bc")) {
		t.Error("different poll and sub pairs collide")
	}
}

// --- Images ---

func TestServedImageHasNoMetadata(t *testing.T) {
	e := newEnv(t)
	const marker = "GPS-PRIVATE-MARKER"
	p, _ := e.poll("Draft", store.StateDraft, 0)
	o := e.login(owner)
	src := testimg.WithEXIF(testimg.JPEG(testimg.Gradient(300, 200, 1)), 6, marker, binary.LittleEndian)
	wantStatus(t, e.upload(o, p.ID, file{"camera.jpg", src}), http.StatusSeeOther)
	items, _ := e.st.Items(bg, p.ID)
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}

	r := e.get(o, "/images/"+items[0].PhotoID)
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.Body, marker) || strings.Contains(r.Body, "Exif") {
		t.Error("metadata survived upload")
	}
	if got := r.Header.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := r.Header.Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Errorf("Cache-Control = %q, want private", got)
	}
	cfg, err := jpeg.DecodeConfig(strings.NewReader(r.Body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 200 || cfg.Height != 300 {
		t.Errorf("size %dx%d, want the orientation tag applied (200x300)", cfg.Width, cfg.Height)
	}
}

func TestImagesAreForMembersOnly(t *testing.T) {
	e := newEnv(t)
	_, items := e.poll("Open", store.StateOpen, 1)
	wantStatus(t, e.get(e.login(voterA), "/images/"+items[0].PhotoID), http.StatusOK)
	wantStatus(t, e.get(newClient(), "/images/"+items[0].PhotoID), http.StatusFound)
	wantStatus(t, e.get(e.login(voterA), "/images/does-not-exist"), http.StatusNotFound)
}

func TestImageIDsAreNotGuessable(t *testing.T) {
	e := newEnv(t)
	_, items := e.poll("Open", store.StateOpen, 2)
	for _, it := range items {
		if len(it.PhotoID) != 32 {
			t.Errorf("photo ID %q isn't 128 random bits in hex", it.PhotoID)
		}
	}
	if items[0].PhotoID == items[1].PhotoID {
		t.Error("IDs repeat")
	}
}

// --- Uploads ---

func TestUploadKeepsGoodFilesAndExplainsBadOnes(t *testing.T) {
	e := newEnv(t)
	p, _ := e.poll("Draft", store.StateDraft, 0)
	o := e.login(owner)
	r := e.upload(o, p.ID,
		file{"good.jpg", testimg.JPEG(testimg.Gradient(60, 40, 1))},
		file{"good.png", testimg.PNG(testimg.Gradient(60, 40, 2))},
		file{"<script>alert(1)</script>.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
		file{"page.jpg", []byte("<!doctype html><script>alert(1)</script>")},
	)
	wantStatus(t, r, http.StatusUnprocessableEntity)
	items, _ := e.st.Items(bg, p.ID)
	if len(items) != 2 {
		t.Errorf("%d items, want the 2 good files", len(items))
	}
	if strings.Contains(r.Body, "<script") {
		t.Error("a filename was shown unescaped")
	}
	if !strings.Contains(r.Body, "isn&#39;t a JPEG or PNG") {
		t.Errorf("no explanation for the bad files: %.500s", r.Body)
	}
	if !strings.Contains(r.Body, "Added 2 photos") {
		t.Error("doesn't say how many were added")
	}
}

func TestUploadRefusesOversizedFile(t *testing.T) {
	e := newEnv(t)
	p, _ := e.poll("Draft", store.StateDraft, 0)
	r := e.upload(e.login(owner), p.ID, file{"big.jpg", make([]byte, maxFileBytes+1)})
	wantStatus(t, r, http.StatusUnprocessableEntity)
	if !strings.Contains(r.Body, "over the 40 MB limit") {
		t.Errorf("no explanation: %.300s", r.Body)
	}
	items, _ := e.st.Items(bg, p.ID)
	if len(items) != 0 {
		t.Errorf("%d items stored", len(items))
	}
}

func TestUploadOnlyToDrafts(t *testing.T) {
	e := newEnv(t)
	p, _ := e.poll("Open", store.StateOpen, 1)
	r := e.upload(e.login(owner), p.ID, file{"late.jpg", testimg.JPEG(testimg.Gradient(60, 40, 1))})
	wantStatus(t, r, http.StatusConflict)
	items, _ := e.st.Items(bg, p.ID)
	if len(items) != 1 {
		t.Errorf("%d items, want the photo refused", len(items))
	}
}

func TestPhotoLimitIsEnforced(t *testing.T) {
	e := newEnv(t)
	p, _ := e.poll("Draft", store.StateDraft, 20) // the limit in newEnv
	r := e.upload(e.login(owner), p.ID, file{"extra.jpg", testimg.JPEG(testimg.Gradient(60, 40, 1))})
	wantStatus(t, r, http.StatusUnprocessableEntity)
	if !strings.Contains(r.Body, "limit was reached") {
		t.Errorf("no explanation: %.300s", r.Body)
	}
}

// --- Owner actions ---

func TestDeletePollNeedsConfirmationAndRemovesEverything(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Closed", store.StateClosed, 1)
	o := e.login(owner)
	wantStatus(t, e.post(o, "/admin/polls/"+p.ID+"/delete", nil), http.StatusBadRequest)
	if _, err := e.st.Poll(bg, p.ID); err != nil {
		t.Fatal("deleted without confirmation")
	}
	wantStatus(t, e.post(o, "/admin/polls/"+p.ID+"/delete", url.Values{"confirm": {"yes"}}), http.StatusSeeOther)
	wantStatus(t, e.get(o, "/admin/polls/"+p.ID), http.StatusNotFound)
	wantStatus(t, e.get(o, "/images/"+items[0].PhotoID), http.StatusNotFound)
}

func TestStateChangesFollowTheOrder(t *testing.T) {
	e := newEnv(t)
	p, _ := e.poll("Draft", store.StateDraft, 1)
	o := e.login(owner)
	wantStatus(t, e.post(o, "/admin/polls/"+p.ID+"/close", nil), http.StatusConflict)
	wantStatus(t, e.post(o, "/admin/polls/"+p.ID+"/open", nil), http.StatusSeeOther)
	wantStatus(t, e.post(o, "/admin/polls/"+p.ID+"/open", nil), http.StatusConflict)
	wantStatus(t, e.post(o, "/admin/polls/"+p.ID+"/close", nil), http.StatusSeeOther)
}

// --- Browser protections ---

func TestCrossSiteFormsAreRefused(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 1)
	voter := e.login(voterA)
	path := "/polls/" + p.ID + "/ratings"
	body := rate(items, 5).Encode()
	const form = "application/x-www-form-urlencoded"

	for name, headers := range map[string]map[string]string{
		"from another site":   {"Sec-Fetch-Site": "cross-site"},
		"from a subdomain":    {"Sec-Fetch-Site": "same-site"},
		"older browser":       {"Origin": "https://evil.example.com"},
		"origin with no host": {"Origin": "null"},
	} {
		t.Run(name, func(t *testing.T) {
			r := e.request(voter, http.MethodPost, path, strings.NewReader(body), form, headers)
			wantStatus(t, r, http.StatusForbidden)
		})
	}
	totals, _, _ := e.st.Totals(bg, p.ID)
	if len(totals) != 0 {
		t.Errorf("a cross-site form was accepted: %v", totals)
	}

	same := e.request(voter, http.MethodPost, path, strings.NewReader(body), form, map[string]string{"Sec-Fetch-Site": "same-origin"})
	wantStatus(t, same, http.StatusSeeOther)
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 1)
	voter := e.login(voterA)
	for _, path := range []string{"/", "/polls/" + p.ID, "/images/" + items[0].PhotoID, "/nope", "/healthz", "/static/style.css"} {
		h := e.get(voter, path).Header
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe") || strings.Contains(csp, "script-src") {
			t.Errorf("%s: CSP = %q", path, csp)
		}
		if !strings.Contains(csp, "form-action 'self' "+e.idp.URL) {
			t.Errorf("%s: CSP doesn't allow the logout redirect: %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: missing a header: %v", path, h)
		}
	}
	if got := e.get(voter, "/").Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("pages have Cache-Control %q, want no-store", got)
	}
}

func TestNoScriptsOnAnyPage(t *testing.T) {
	e := newEnv(t)
	p, _ := e.poll("Open", store.StateOpen, 1)
	closed, _ := e.poll("Closed", store.StateClosed, 1)
	draft, _ := e.poll("Draft", store.StateDraft, 1)
	v, o := e.login(voterA), e.login(owner)
	pages := []string{
		e.get(v, "/").Body,
		e.get(v, "/polls/"+p.ID).Body,
		e.get(v, "/polls/"+closed.ID).Body,
		e.get(o, "/admin").Body,
		e.get(o, "/admin/polls/"+draft.ID).Body,
		e.get(o, "/admin/polls/"+p.ID).Body,
		e.get(v, "/nope").Body,
	}
	for _, page := range pages {
		if strings.Contains(page, "<script") || strings.Contains(page, "onclick") {
			t.Errorf("found a script: %.200s", page)
		}
	}
}

func TestOversizedFormIsRefused(t *testing.T) {
	e := newEnv(t)
	p, items := e.poll("Open", store.StateOpen, 1)
	form := url.Values{"rating_" + items[0].ID: {"3"}, "padding": {strings.Repeat("x", maxFormBytes)}}
	wantStatus(t, e.post(e.login(voterA), "/polls/"+p.ID+"/ratings", form), http.StatusBadRequest)
}

func TestHealthCheckIsPublicAndSaysNothing(t *testing.T) {
	e := newEnv(t)
	r := e.get(newClient(), "/healthz")
	wantStatus(t, r, http.StatusOK)
	if r.Body != "ok\n" {
		t.Errorf("body = %q", r.Body)
	}
}

func TestLoginUnavailableWhenProviderIsDown(t *testing.T) {
	e := newEnv(t)
	e.idp.Close()
	wantStatus(t, e.get(newClient(), "/login"), http.StatusServiceUnavailable)
	wantStatus(t, e.get(newClient(), "/healthz"), http.StatusOK)
}
