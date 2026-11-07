// Command devserver runs the app on this machine with a fake login and generated
// photos, for trying it out. It's for development only and isn't in the image.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sander-skjulsvik/photo-vote/internal/auth"
	"github.com/sander-skjulsvik/photo-vote/internal/imaging"
	"github.com/sander-skjulsvik/photo-vote/internal/store"
	"github.com/sander-skjulsvik/photo-vote/internal/testidp"
	"github.com/sander-skjulsvik/photo-vote/internal/testimg"
	"github.com/sander-skjulsvik/photo-vote/internal/web"
)

const (
	voterGroup = "dev-voters"
	ownerGroup = "dev-owners"
	clientID   = "photo-vote-dev"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "address for the app")
	idpAddr := flag.String("idp-addr", "127.0.0.1:9096", "address for the fake login provider")
	flag.Parse()

	dir, err := os.MkdirTemp("", "photo-vote-dev-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	idp, err := testidp.Start(*idpAddr, clientID, []testidp.User{
		{Sub: "dev-owner", Label: "Owner", Groups: []string{ownerGroup}},
		{Sub: "dev-voter-1", Label: "Voter 1", Groups: []string{voterGroup}},
		{Sub: "dev-voter-2", Label: "Voter 2", Groups: []string{voterGroup}},
		{Sub: "dev-both", Label: "Owner who also votes", Groups: []string{ownerGroup, voterGroup}},
		{Sub: "dev-outsider", Label: "Someone with no access", Groups: []string{"other"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer idp.Close()

	st, err := store.Open(filepath.Join(dir, "dev.db"), store.Limits{MaxItemsPerPoll: 50, MaxStorageBytes: 1 << 30})
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if err := seed(st); err != nil {
		log.Fatal(err)
	}

	baseURL := "http://" + *addr
	am := auth.NewManager(auth.Config{
		Issuer:       idp.URL,
		ClientID:     clientID,
		ClientSecret: testidp.ClientSecret,
		RedirectURL:  baseURL + "/auth/callback",
		VoterGroup:   voterGroup,
		OwnerGroup:   ownerGroup,
		SessionTTL:   web.SessionTTL,
	})
	srv, err := web.New(web.Config{
		BaseURL:     baseURL,
		Issuer:      idp.URL,
		VoterSecret: []byte(strings.Repeat("dev-only", 4)),
		Logger:      slog.New(slog.NewTextHandler(os.Stdout, nil)),
	}, st, am)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Open %s and pick a fake user. Data is thrown away on exit.\n", baseURL)
	server := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(server.ListenAndServe())
}

// seed creates one poll in each state, using generated pictures.
func seed(st *store.Store) error {
	ctx := context.Background()
	descriptions := []string{
		"Generated picture with a short description, as a voter would see it.",
		"",
		"Another description, a little longer, to show how the text wraps underneath a photo on a narrow phone screen.",
		"",
		"Should this be the first one people see?",
		"",
	}
	make3 := func(title string, n int) (store.Poll, []store.Item, error) {
		p, err := st.CreatePoll(ctx, title)
		if err != nil {
			return store.Poll{}, nil, err
		}
		items := make([]store.Item, 0, n)
		for i := range n {
			w, h := 1200, 800
			if i%3 == 1 {
				w, h = 800, 1200
			}
			res, err := imaging.Process(testimg.JPEG(testimg.Gradient(w, h, i+1)))
			if err != nil {
				return store.Poll{}, nil, err
			}
			it, err := st.AddPhoto(ctx, p.ID, store.Photo{JPEG: res.JPEG, Width: res.Width, Height: res.Height})
			if err != nil {
				return store.Poll{}, nil, err
			}
			if _, err := st.SetDescription(ctx, it.ID, descriptions[i%len(descriptions)]); err != nil {
				return store.Poll{}, nil, err
			}
			items = append(items, it)
		}
		return p, items, nil
	}

	if _, _, err := make3("A draft poll", 2); err != nil {
		return err
	}
	open, _, err := make3("An open poll", 6)
	if err != nil {
		return err
	}
	if err := st.SetState(ctx, open.ID, store.StateDraft, store.StateOpen); err != nil {
		return err
	}
	closed, items, err := make3("A closed poll", 4)
	if err != nil {
		return err
	}
	if err := st.SetState(ctx, closed.ID, store.StateDraft, store.StateOpen); err != nil {
		return err
	}
	for v, scores := range [][]int{{5, 3, 1, 0}, {4, 2, 0, 0}, {5, 4, 2, 0}} {
		ratings := map[string]int{}
		for i, score := range scores {
			ratings[items[i].ID] = score
		}
		if err := st.SaveRatings(ctx, closed.ID, []byte(fmt.Sprintf("seed-%d", v)), ratings); err != nil {
			return err
		}
	}
	return st.SetState(ctx, closed.ID, store.StateOpen, store.StateClosed)
}
