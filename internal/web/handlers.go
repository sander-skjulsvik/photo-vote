package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sander-skjulsvik/photo-vote/internal/auth"
	"github.com/sander-skjulsvik/photo-vote/internal/imaging"
	"github.com/sander-skjulsvik/photo-vote/internal/store"
)

const (
	maxTitleRunes       = 120
	maxDescriptionRunes = 280
	maxFileBytes        = 40 << 20
	maxUploadBytes      = 600 << 20
	uploadTimeout       = 15 * time.Minute
	flowCookieAge       = 10 * time.Minute
)

// --- Templates ---

var pageNames = []string{"home", "poll", "results", "admin", "admin_poll", "error", "signed_out"}

func parsePages() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"average": func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
		"people": func(n int) string {
			if n == 1 {
				return "1 person"
			}
			return strconv.Itoa(n) + " people"
		},
		"plural": func(n int, word string) string {
			if n == 1 {
				return "1 " + word
			}
			return strconv.Itoa(n) + " " + word + "s"
		},
	}
	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}

type pageData struct {
	Title    string
	LoggedIn bool
	Owner    bool
	Data     any
}

// render builds the page before writing, so a template error can't leave half a
// page behind.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) {
	id := identityFrom(r)
	var buf bytes.Buffer
	err := s.pages[page].Execute(&buf, pageData{
		Title:    title,
		LoggedIn: id.Sub != "",
		Owner:    id.Owner,
		Data:     data,
	})
	if err != nil {
		s.log.Error("render failed", "page", page)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

type errorData struct {
	Status  int
	Message string
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, status, "error", http.StatusText(status), errorData{status, message})
}

// --- Public routes ---

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

func (s *Server) stylesheet(w http.ResponseWriter, _ *http.Request) {
	data, err := fs.ReadFile(assets, "static/style.css")
	if err != nil {
		http.Error(w, "missing", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if _, ok := s.auth.Session(s.cookieValue(r, sessionCookie)); ok {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	authURL, flowID, err := s.auth.Begin(next)
	if err != nil {
		s.log.Error("login unavailable", "error", err.Error())
		s.renderError(w, r, http.StatusServiceUnavailable, "Login isn't available right now. Please try again in a minute.")
		return
	}
	s.setCookie(w, flowCookie, flowID, flowCookieAge)
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	flowID := s.cookieValue(r, flowCookie)
	s.clearCookie(w, flowCookie)
	q := r.URL.Query()

	if q.Get("error") != "" {
		s.renderError(w, r, http.StatusForbidden, "The login was refused. If you think you should have access, ask whoever invited you.")
		return
	}
	token, next, err := s.auth.Complete(r.Context(), flowID, q.Get("state"), q.Get("code"))
	if errors.Is(err, auth.ErrNoAccess) {
		s.renderError(w, r, http.StatusForbidden, "You're logged in, but your account doesn't have access to this site.")
		return
	}
	if err != nil {
		// Whether this was a stale tab or something worse, people get the same
		// answer. The log says why, to make setup problems diagnosable. It holds
		// only technical reasons, never who was logging in.
		var loginErr *auth.LoginError
		if errors.As(err, &loginErr) {
			s.log.Warn("login attempt rejected", "reason", loginErr.Reason, "detail", loginErr.Detail)
		} else {
			s.log.Error("login unavailable", "error", err.Error())
		}
		s.renderError(w, r, http.StatusBadRequest, "That login didn't work. Please go back to the site and try again.")
		return
	}
	s.setCookie(w, sessionCookie, token, SessionTTL)
	http.Redirect(w, r, safeNext(next), http.StatusFound)
}

// SessionTTL is how long a login lasts. Removing someone from a group takes
// effect at the latest when their session ends.
const SessionTTL = 12 * time.Hour

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.End(s.cookieValue(r, sessionCookie))
	s.clearCookie(w, sessionCookie)
	target := s.auth.EndSessionURL()
	if target == "" {
		target = "/signed-out"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) signedOut(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "signed_out", "Signed out", nil)
}

// --- Voter pages ---

type homeData struct {
	Open, Closed []store.Poll
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	open, err := s.store.Polls(r.Context(), store.StateOpen)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	closed, err := s.store.Polls(r.Context(), store.StateClosed)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "home", "Polls", homeData{open, closed})
}

// memberPoll loads a poll people may see. A draft looks exactly like a poll that
// doesn't exist, even to the owner, whose drafts are on the admin pages.
func (s *Server) memberPoll(w http.ResponseWriter, r *http.Request) (store.Poll, bool) {
	p, err := s.store.Poll(r.Context(), r.PathValue("id"))
	if err == nil && p.State == store.StateDraft {
		err = store.ErrNotFound
	}
	if err != nil {
		s.fail(w, r, err)
		return store.Poll{}, false
	}
	return p, true
}

type starOption struct {
	Value   int
	Label   string
	Checked bool
}

type voteItem struct {
	store.Item
	Number int
	Score  int
	Stars  []starOption
}

type pollData struct {
	Poll    store.Poll
	Items   []voteItem
	Rated   int
	CanRate bool
	Saved   bool
}

func (s *Server) pollPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.memberPoll(w, r)
	if !ok {
		return
	}
	if p.State == store.StateClosed {
		s.resultsPage(w, r, p)
		return
	}

	id := identityFrom(r)
	items, err := s.store.Items(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	scores := map[string]int{}
	if id.Voter {
		scores, err = s.store.VoterRatings(r.Context(), p.ID, s.voterHash(p.ID, id))
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}

	data := pollData{Poll: p, CanRate: id.Voter, Rated: len(scores), Saved: r.URL.Query().Get("saved") == "1"}
	for n, it := range items {
		v := voteItem{Item: it, Number: n + 1, Score: scores[it.ID]}
		// Highest first: the stylesheet shows them in reverse, which lets plain
		// CSS light up every star up to the chosen one.
		for value := 5; value >= 1; value-- {
			v.Stars = append(v.Stars, starOption{
				Value:   value,
				Label:   fmt.Sprintf("%d out of 5", value),
				Checked: v.Score == value,
			})
		}
		data.Items = append(data.Items, v)
	}
	s.render(w, r, http.StatusOK, "poll", p.Title, data)
}

type resultItem struct {
	store.Item
	Number  int
	Count   int
	Average float64
	Own     int
}

type resultsData struct {
	Poll   store.Poll
	Items  []resultItem
	Voters int
	Voter  bool
}

func (s *Server) resultsPage(w http.ResponseWriter, r *http.Request, p store.Poll) {
	id := identityFrom(r)
	items, totals, voters, err := s.itemsWithTotals(r, p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	own := map[string]int{}
	if id.Voter {
		own, err = s.store.VoterRatings(r.Context(), p.ID, s.voterHash(p.ID, id))
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}
	data := resultsData{Poll: p, Voters: voters, Voter: id.Voter}
	for n, it := range items {
		t := totals[it.ID]
		data.Items = append(data.Items, resultItem{Item: it, Number: n + 1, Count: t.Count, Average: t.Average, Own: own[it.ID]})
	}
	sort.SliceStable(data.Items, func(i, j int) bool {
		return data.Items[i].Average > data.Items[j].Average
	})
	s.render(w, r, http.StatusOK, "results", p.Title, data)
}

func (s *Server) itemsWithTotals(r *http.Request, pollID string) ([]store.Item, map[string]store.Total, int, error) {
	items, err := s.store.Items(r.Context(), pollID)
	if err != nil {
		return nil, nil, 0, err
	}
	totals, voters, err := s.store.Totals(r.Context(), pollID)
	if err != nil {
		return nil, nil, 0, err
	}
	return items, totals, voters, nil
}

func (s *Server) voterHash(pollID string, id auth.Identity) []byte {
	return VoterHash(s.cfg.VoterSecret, pollID, id.Sub)
}

func (s *Server) saveRatings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.memberPoll(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That form couldn't be read.")
		return
	}
	items, err := s.store.Items(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Only this poll's items are read from the form, so anything else in it is
	// ignored. A missing field leaves that rating as it is.
	scores := make(map[string]int, len(items))
	for _, it := range items {
		field := "rating_" + it.ID
		if !r.PostForm.Has(field) {
			continue
		}
		score, err := strconv.Atoi(r.PostForm.Get(field))
		if err != nil || score < 0 || score > 5 {
			s.renderError(w, r, http.StatusBadRequest, "A rating has to be between 1 and 5 stars.")
			return
		}
		scores[it.ID] = score
	}
	err = s.store.SaveRatings(r.Context(), p.ID, s.voterHash(p.ID, identityFrom(r)), scores)
	if errors.Is(err, store.ErrConflict) {
		s.renderError(w, r, http.StatusConflict, "This poll has closed, so your last changes weren't saved.")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/polls/"+p.ID+"?saved=1", http.StatusSeeOther)
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	photo, err := s.store.Photo(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Photos in a draft are only for the owner, and look missing to anyone else.
	if photo.PollState == store.StateDraft && !identityFrom(r).Owner {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(photo.JPEG)))
	// Only the person's own browser may keep a copy, and not for long.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(photo.JPEG)
}

// --- Owner pages ---

type adminHomeData struct {
	Polls []store.Poll
}

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request) {
	polls, err := s.store.Polls(r.Context(), store.StateDraft, store.StateOpen, store.StateClosed)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin", "Manage polls", adminHomeData{polls})
}

func (s *Server) adminCreatePoll(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That form couldn't be read.")
		return
	}
	title := strings.TrimSpace(r.PostForm.Get("title"))
	if title == "" || utf8.RuneCountInString(title) > maxTitleRunes {
		s.renderError(w, r, http.StatusBadRequest, fmt.Sprintf("A poll title needs 1 to %d characters.", maxTitleRunes))
		return
	}
	p, err := s.store.CreatePoll(r.Context(), title)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/polls/"+p.ID, http.StatusSeeOther)
}

type adminItem struct {
	store.Item
	Number  int
	Count   int
	Average float64
}

type adminPollData struct {
	Poll           store.Poll
	Items          []adminItem
	Voters         int
	ShowTotals     bool
	CanUpload      bool
	Editable       bool
	MaxDescription int
	Added          int
	Failures       []string
}

func (s *Server) adminPoll(w http.ResponseWriter, r *http.Request) {
	added, _ := strconv.Atoi(r.URL.Query().Get("added"))
	s.renderAdminPoll(w, r, http.StatusOK, added, nil)
}

func (s *Server) renderAdminPoll(w http.ResponseWriter, r *http.Request, status, added int, failures []string) {
	p, err := s.store.Poll(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items, totals, voters, err := s.itemsWithTotals(r, p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := adminPollData{
		Poll:           p,
		Voters:         voters,
		ShowTotals:     p.State != store.StateDraft,
		CanUpload:      p.State == store.StateDraft,
		Editable:       p.State != store.StateClosed,
		MaxDescription: maxDescriptionRunes,
		Added:          added,
		Failures:       failures,
	}
	for n, it := range items {
		t := totals[it.ID]
		data.Items = append(data.Items, adminItem{Item: it, Number: n + 1, Count: t.Count, Average: t.Average})
	}
	s.render(w, r, status, "admin_poll", p.Title, data)
}

func (s *Server) adminUpload(w http.ResponseWriter, r *http.Request) {
	pollID := r.PathValue("id")
	p, err := s.store.Poll(r.Context(), pollID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if p.State != store.StateDraft {
		s.fail(w, r, store.ErrConflict)
		return
	}

	// Uploads over a slow connection can outlast the server's normal timeouts.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(uploadTimeout))
	rc.SetWriteDeadline(time.Now().Add(uploadTimeout))

	// Decoding is memory-hungry, so only one upload runs at a time.
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	case <-r.Context().Done():
		return
	}

	// Parts are read one at a time, so nothing spills to temp files on the
	// read-only filesystem and only one photo is in memory at once.
	reader, err := r.MultipartReader()
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That upload couldn't be read.")
		return
	}
	added := 0
	var failures []string
	fail := func(name, reason string) {
		failures = append(failures, name+": "+reason)
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				failures = append(failures, "The upload was too large, so the remaining files weren't read.")
			} else {
				failures = append(failures, "The upload was cut off, so the remaining files weren't read.")
			}
			break
		}
		name := part.FileName()
		if part.FormName() != "photos" || name == "" {
			part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxFileBytes+1))
		part.Close()
		if err != nil {
			fail(name, "couldn't be read")
			break
		}
		if len(data) > maxFileBytes {
			fail(name, fmt.Sprintf("is over the %d MB limit", maxFileBytes>>20))
			continue
		}
		result, err := imaging.Process(data)
		switch {
		case errors.Is(err, imaging.ErrUnsupported):
			fail(name, "isn't a JPEG or PNG")
			continue
		case errors.Is(err, imaging.ErrTooLarge):
			fail(name, "has too many pixels")
			continue
		case err != nil:
			fail(name, "couldn't be processed")
			continue
		}
		_, err = s.store.AddPhoto(r.Context(), pollID, store.Photo{JPEG: result.JPEG, Width: result.Width, Height: result.Height})
		switch {
		case errors.Is(err, store.ErrLimit):
			fail(name, "wasn't added, because a photo or storage limit was reached")
		case errors.Is(err, store.ErrConflict):
			fail(name, "wasn't added, because the poll is no longer a draft")
		case err != nil:
			fail(name, "couldn't be saved")
		default:
			added++
		}
	}

	if len(failures) > 0 {
		s.renderAdminPoll(w, r, http.StatusUnprocessableEntity, added, failures)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/polls/%s?added=%d", pollID, added), http.StatusSeeOther)
}

func (s *Server) adminSetState(from, to store.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := s.store.SetState(r.Context(), id, from, to); err != nil {
			s.fail(w, r, err)
			return
		}
		http.Redirect(w, r, "/admin/polls/"+id, http.StatusSeeOther)
	}
}

func (s *Server) adminDeletePoll(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That form couldn't be read.")
		return
	}
	if r.PostForm.Get("confirm") != "yes" {
		s.renderError(w, r, http.StatusBadRequest, "Tick the box to confirm that you want to delete this poll for good.")
		return
	}
	if err := s.store.DeletePoll(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminSetDescription(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That form couldn't be read.")
		return
	}
	description := strings.TrimSpace(r.PostForm.Get("description"))
	if utf8.RuneCountInString(description) > maxDescriptionRunes {
		s.renderError(w, r, http.StatusBadRequest, fmt.Sprintf("A description can have up to %d characters.", maxDescriptionRunes))
		return
	}
	pollID, err := s.store.SetDescription(r.Context(), r.PathValue("id"), description)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/polls/"+pollID, http.StatusSeeOther)
}

func (s *Server) adminDeleteItem(w http.ResponseWriter, r *http.Request) {
	pollID, err := s.store.DeleteItem(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/polls/"+pollID, http.StatusSeeOther)
}
