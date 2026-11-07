package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var ctx = context.Background()

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, Limits{MaxItemsPerPoll: 3, MaxStorageBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func pollWithItems(t *testing.T, s *Store, state State, n int) (Poll, []Item) {
	t.Helper()
	p, err := s.CreatePoll(ctx, "A poll")
	if err != nil {
		t.Fatal(err)
	}
	items := make([]Item, n)
	for i := range items {
		items[i], err = s.AddPhoto(ctx, p.ID, Photo{JPEG: []byte("image"), Width: 10, Height: 20})
		if err != nil {
			t.Fatal(err)
		}
	}
	if state != StateDraft {
		if err := s.SetState(ctx, p.ID, StateDraft, StateOpen); err != nil {
			t.Fatal(err)
		}
	}
	if state == StateClosed {
		if err := s.SetState(ctx, p.ID, StateOpen, StateClosed); err != nil {
			t.Fatal(err)
		}
	}
	p.State = state
	return p, items
}

func TestRatingsReplaceAndClear(t *testing.T) {
	s, _ := open(t)
	p, items := pollWithItems(t, s, StateOpen, 2)
	voter := []byte("voter")
	a, b := items[0].ID, items[1].ID

	if err := s.SaveRatings(ctx, p.ID, voter, map[string]int{a: 3, b: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRatings(ctx, p.ID, voter, map[string]int{a: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := s.VoterRatings(ctx, p.ID, voter)
	if err != nil {
		t.Fatal(err)
	}
	if got[a] != 1 || got[b] != 5 {
		t.Errorf("ratings = %v, want the first replaced and the second untouched", got)
	}
	totals, voters, err := s.Totals(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if totals[a].Count != 1 || voters != 1 {
		t.Errorf("changing a rating added a vote: totals %v, voters %d", totals, voters)
	}

	if err := s.SaveRatings(ctx, p.ID, voter, map[string]int{a: 0}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.VoterRatings(ctx, p.ID, voter)
	if _, ok := got[a]; ok {
		t.Errorf("score 0 didn't clear the rating: %v", got)
	}
}

func TestTotalsAcrossVoters(t *testing.T) {
	s, _ := open(t)
	p, items := pollWithItems(t, s, StateOpen, 2)
	a, b := items[0].ID, items[1].ID
	for voter, score := range map[string]int{"one": 2, "two": 5} {
		if err := s.SaveRatings(ctx, p.ID, []byte(voter), map[string]int{a: score}); err != nil {
			t.Fatal(err)
		}
	}
	totals, voters, err := s.Totals(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if voters != 2 || totals[a].Count != 2 || totals[a].Average != 3.5 {
		t.Errorf("voters %d, total %+v; want 2 voters, count 2, average 3.5", voters, totals[a])
	}
	if _, ok := totals[b]; ok {
		t.Errorf("an unrated item has a total: %+v", totals[b])
	}
}

func TestRatingsOnlyWhileOpen(t *testing.T) {
	s, _ := open(t)
	for _, state := range []State{StateDraft, StateClosed} {
		p, items := pollWithItems(t, s, state, 1)
		err := s.SaveRatings(ctx, p.ID, []byte("voter"), map[string]int{items[0].ID: 3})
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s poll: err = %v, want ErrConflict", state, err)
		}
	}
}

func TestRatingsRejectInvalid(t *testing.T) {
	s, _ := open(t)
	p, items := pollWithItems(t, s, StateOpen, 1)
	other, otherItems := pollWithItems(t, s, StateOpen, 1)

	if err := s.SaveRatings(ctx, p.ID, []byte("v"), map[string]int{otherItems[0].ID: 3}); !errors.Is(err, ErrNotFound) {
		t.Errorf("item from another poll: err = %v, want ErrNotFound", err)
	}
	if err := s.SaveRatings(ctx, p.ID, []byte("v"), map[string]int{items[0].ID: 6}); err == nil {
		t.Error("score 6 was accepted")
	}
	totals, _, _ := s.Totals(ctx, other.ID)
	if len(totals) != 0 {
		t.Errorf("rejected rating was stored: %v", totals)
	}
}

func TestPhotosOnlyChangeInDraft(t *testing.T) {
	s, _ := open(t)
	p, items := pollWithItems(t, s, StateOpen, 1)
	if _, err := s.AddPhoto(ctx, p.ID, Photo{JPEG: []byte("x")}); !errors.Is(err, ErrConflict) {
		t.Errorf("adding to an open poll: err = %v, want ErrConflict", err)
	}
	if _, err := s.DeleteItem(ctx, items[0].ID); !errors.Is(err, ErrConflict) {
		t.Errorf("deleting from an open poll: err = %v, want ErrConflict", err)
	}
}

func TestDescriptionLockedOnceClosed(t *testing.T) {
	s, _ := open(t)
	open, openItems := pollWithItems(t, s, StateOpen, 1)
	if _, err := s.SetDescription(ctx, openItems[0].ID, "fixed a typo"); err != nil {
		t.Errorf("open poll: %v", err)
	}
	got, _ := s.Items(ctx, open.ID)
	if got[0].Description != "fixed a typo" {
		t.Errorf("description = %q", got[0].Description)
	}
	_, closedItems := pollWithItems(t, s, StateClosed, 1)
	if _, err := s.SetDescription(ctx, closedItems[0].ID, "new"); !errors.Is(err, ErrConflict) {
		t.Errorf("closed poll: err = %v, want ErrConflict", err)
	}
}

func TestLimits(t *testing.T) {
	s, _ := open(t)
	p, _ := pollWithItems(t, s, StateDraft, 3)
	if _, err := s.AddPhoto(ctx, p.ID, Photo{JPEG: []byte("x")}); !errors.Is(err, ErrLimit) {
		t.Errorf("fourth photo: err = %v, want ErrLimit", err)
	}
	q, _ := pollWithItems(t, s, StateDraft, 0)
	big := make([]byte, 1<<20)
	if _, err := s.AddPhoto(ctx, q.ID, Photo{JPEG: big}); !errors.Is(err, ErrLimit) {
		t.Errorf("over the storage limit: err = %v, want ErrLimit", err)
	}
}

func TestSetStateRejectsWrongTransition(t *testing.T) {
	s, _ := open(t)
	p, _ := pollWithItems(t, s, StateDraft, 0)
	if err := s.SetState(ctx, p.ID, StateOpen, StateClosed); !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
	if err := s.SetState(ctx, "missing", StateDraft, StateOpen); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDeletePollErasesDataFromDisk(t *testing.T) {
	s, path := open(t)
	marker := []byte("UNIQUE-PHOTO-BYTES-THAT-MUST-NOT-SURVIVE")
	p, _ := pollWithItems(t, s, StateDraft, 0)
	photo := append(bytes.Repeat([]byte{0xAB}, 8000), marker...) // spans overflow pages
	item, err := s.AddPhoto(ctx, p.ID, Photo{JPEG: photo})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetState(ctx, p.ID, StateDraft, StateOpen); err != nil {
		t.Fatal(err)
	}
	voter := []byte("VOTER-HASH-MARKER")
	if err := s.SaveRatings(ctx, p.ID, voter, map[string]int{item.ID: 4}); err != nil {
		t.Fatal(err)
	}
	if !fileContains(t, path, marker) {
		t.Fatal("test is broken: the marker was never on disk")
	}

	if err := s.DeletePoll(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	for _, needle := range [][]byte{marker, voter, []byte(p.ID)} {
		if fileContains(t, path, needle) {
			t.Errorf("%q is still on disk after deleting the poll", needle)
		}
	}
	if _, err := s.Photo(ctx, item.PhotoID); !errors.Is(err, ErrNotFound) {
		t.Errorf("photo still readable: %v", err)
	}
	if err := s.DeletePoll(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

// fileContains looks through the database and its write-ahead log.
func fileContains(t *testing.T, path string, needle []byte) bool {
	t.Helper()
	// Flush the WAL contents into view by reading both files as they are.
	for _, p := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, needle) {
			return true
		}
	}
	return false
}

func TestReopenKeepsSchemaAndData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	limits := Limits{MaxItemsPerPoll: 3, MaxStorageBytes: 1 << 20}
	s, err := Open(path, limits)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePoll(ctx, "Keep me")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Poll(ctx, p.ID)
	if err != nil || got.Title != "Keep me" {
		t.Errorf("poll after reopen = %+v, err %v", got, err)
	}
}
