// Package store keeps polls, photos and ratings in a single SQLite file.
//
// Photos are stored as blobs rather than files, so deleting a poll removes its
// photos and ratings in one transaction. secure_delete overwrites the freed
// pages, and DeletePoll truncates the write-ahead log, so deleted data doesn't
// linger on disk.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound means the requested poll, item or photo doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means the poll's state doesn't allow the change.
	ErrConflict = errors.New("not allowed in this poll state")
	// ErrLimit means an item or storage limit would be exceeded.
	ErrLimit = errors.New("limit reached")
)

// State is where a poll is in its life.
type State string

const (
	// StateDraft polls are visible only to the owner.
	StateDraft State = "draft"
	// StateOpen polls accept ratings.
	StateOpen State = "open"
	// StateClosed polls show results to voters.
	StateClosed State = "closed"
)

// Limits bound how much data the store accepts.
type Limits struct {
	MaxItemsPerPoll int
	MaxStorageBytes int64
}

// Poll is a set of items that voters rate.
type Poll struct {
	ID        string
	Title     string
	State     State
	CreatedAt time.Time
}

// Item is the thing being rated. It has one photo for now, and is separate
// from the photo so a profile with several photos can become an item later.
type Item struct {
	ID          string
	PollID      string
	Description string
	PhotoID     string
	Width       int
	Height      int
}

// Photo is an already processed image.
type Photo struct {
	JPEG   []byte
	Width  int
	Height int
}

// StoredPhoto is a photo as read back, with its poll's state for access checks.
type StoredPhoto struct {
	JPEG      []byte
	PollState State
}

// Total is an item's aggregate result. There is deliberately no breakdown by
// score, because with few voters it would give people away.
type Total struct {
	Count   int
	Average float64
}

// Store is the database handle.
type Store struct {
	db     *sql.DB
	limits Limits
}

var migrations = []string{`
CREATE TABLE polls (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL,
	state      TEXT NOT NULL CHECK (state IN ('draft', 'open', 'closed')),
	created_at INTEGER NOT NULL
);
CREATE TABLE items (
	id          TEXT PRIMARY KEY,
	poll_id     TEXT NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
	position    INTEGER NOT NULL,
	description TEXT NOT NULL DEFAULT ''
);
CREATE INDEX items_poll ON items(poll_id, position);
CREATE TABLE photos (
	id      TEXT PRIMARY KEY,
	item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	width   INTEGER NOT NULL,
	height  INTEGER NOT NULL,
	jpeg    BLOB NOT NULL
);
CREATE INDEX photos_item ON photos(item_id);
-- WITHOUT ROWID and no timestamp: rows sort by the keyed hash, so nothing
-- stored reveals the order in which people voted.
CREATE TABLE ratings (
	voter_hash BLOB NOT NULL,
	item_id    TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	score      INTEGER NOT NULL CHECK (score BETWEEN 1 AND 5),
	PRIMARY KEY (voter_hash, item_id)
) WITHOUT ROWID;
CREATE INDEX ratings_item ON ratings(item_id);
`}

// Open opens the database at path and brings its schema up to date.
func Open(path string, limits Limits) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=secure_delete(1)" +
		// The root filesystem is read-only, so temp data can't go to files.
		"&_pragma=temp_store(2)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One connection serialises all access. Traffic is tiny, and it rules out
	// lock errors. A query must never run outside an open transaction, or it
	// would wait forever for this connection.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, limits: limits}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema %d is newer than this app supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		err := s.inTx(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return err
			}
			_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migrate to schema %d: %w", i+1, err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database responds.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
	return hex.EncodeToString(b)
}

// CreatePoll adds a new draft poll.
func (s *Store) CreatePoll(ctx context.Context, title string) (Poll, error) {
	p := Poll{ID: newID(), Title: title, State: StateDraft, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO polls (id, title, state, created_at) VALUES (?, ?, ?, ?)`,
		p.ID, p.Title, p.State, p.CreatedAt.Unix())
	if err != nil {
		return Poll{}, fmt.Errorf("create poll: %w", err)
	}
	return p, nil
}

// Polls lists polls in the given states, newest first.
func (s *Store) Polls(ctx context.Context, states ...State) ([]Poll, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, state, created_at FROM polls ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list polls: %w", err)
	}
	defer rows.Close()

	want := make(map[State]bool, len(states))
	for _, st := range states {
		want[st] = true
	}
	var polls []Poll
	for rows.Next() {
		var p Poll
		var created int64
		if err := rows.Scan(&p.ID, &p.Title, &p.State, &created); err != nil {
			return nil, fmt.Errorf("read poll: %w", err)
		}
		p.CreatedAt = time.Unix(created, 0).UTC()
		if want[p.State] {
			polls = append(polls, p)
		}
	}
	return polls, rows.Err()
}

// Poll returns one poll.
func (s *Store) Poll(ctx context.Context, id string) (Poll, error) {
	var p Poll
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, state, created_at FROM polls WHERE id = ?`, id,
	).Scan(&p.ID, &p.Title, &p.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Poll{}, ErrNotFound
	}
	if err != nil {
		return Poll{}, fmt.Errorf("read poll: %w", err)
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	return p, nil
}

// SetState moves a poll from one state to another. It returns ErrConflict if
// the poll isn't in the expected state.
func (s *Store) SetState(ctx context.Context, id string, from, to State) error {
	res, err := s.db.ExecContext(ctx, `UPDATE polls SET state = ? WHERE id = ? AND state = ?`, to, id, from)
	if err != nil {
		return fmt.Errorf("set poll state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set poll state: %w", err)
	}
	if n == 1 {
		return nil
	}
	if _, err := s.Poll(ctx, id); err != nil {
		return err
	}
	return ErrConflict
}

// DeletePoll permanently removes a poll with its photos and ratings.
func (s *Store) DeletePoll(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM polls WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete poll: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete poll: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	// Move the deletion into the main file and empty the log, which would
	// otherwise still hold copies of the deleted pages.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("clear write-ahead log: %w", err)
	}
	return nil
}

// AddPhoto adds a photo as a new item. The poll must still be a draft.
func (s *Store) AddPhoto(ctx context.Context, pollID string, photo Photo) (Item, error) {
	item := Item{ID: newID(), PollID: pollID, PhotoID: newID(), Width: photo.Width, Height: photo.Height}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var state State
		err := tx.QueryRowContext(ctx, `SELECT state FROM polls WHERE id = ?`, pollID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != StateDraft {
			return ErrConflict
		}

		var count, nextPosition int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(position), 0) + 1 FROM items WHERE poll_id = ?`, pollID,
		).Scan(&count, &nextPosition)
		if err != nil {
			return err
		}
		if count >= s.limits.MaxItemsPerPoll {
			return ErrLimit
		}

		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(jpeg)), 0) FROM photos`).Scan(&used); err != nil {
			return err
		}
		if used+int64(len(photo.JPEG)) > s.limits.MaxStorageBytes {
			return ErrLimit
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO items (id, poll_id, position) VALUES (?, ?, ?)`, item.ID, pollID, nextPosition); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO photos (id, item_id, width, height, jpeg) VALUES (?, ?, ?, ?, ?)`,
			item.PhotoID, item.ID, photo.Width, photo.Height, photo.JPEG)
		return err
	})
	if err != nil {
		return Item{}, fmt.Errorf("add photo: %w", err)
	}
	return item, nil
}

// Items lists a poll's items in upload order, without the image data.
func (s *Store) Items(ctx context.Context, pollID string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.poll_id, i.description, p.id, p.width, p.height
		FROM items i JOIN photos p ON p.item_id = i.id
		WHERE i.poll_id = ?
		ORDER BY i.position`, pollID)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.PollID, &it.Description, &it.PhotoID, &it.Width, &it.Height); err != nil {
			return nil, fmt.Errorf("read item: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// SetDescription changes an item's description. Closed polls can't change,
// because the results were given against the old text.
func (s *Store) SetDescription(ctx context.Context, itemID, description string) (pollID string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var state State
		if err := itemPoll(ctx, tx, itemID, &pollID, &state); err != nil {
			return err
		}
		if state == StateClosed {
			return ErrConflict
		}
		_, err := tx.ExecContext(ctx, `UPDATE items SET description = ? WHERE id = ?`, description, itemID)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("set description: %w", err)
	}
	return pollID, nil
}

// DeleteItem removes an item and its photo. The poll must still be a draft, so
// a photo can't disappear after people have started rating.
func (s *Store) DeleteItem(ctx context.Context, itemID string) (pollID string, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var state State
		if err := itemPoll(ctx, tx, itemID, &pollID, &state); err != nil {
			return err
		}
		if state != StateDraft {
			return ErrConflict
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM items WHERE id = ?`, itemID)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("delete item: %w", err)
	}
	return pollID, nil
}

func itemPoll(ctx context.Context, tx *sql.Tx, itemID string, pollID *string, state *State) error {
	err := tx.QueryRowContext(ctx, `
		SELECT p.id, p.state FROM items i JOIN polls p ON p.id = i.poll_id WHERE i.id = ?`, itemID,
	).Scan(pollID, state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Photo returns an image and its poll's state.
func (s *Store) Photo(ctx context.Context, photoID string) (StoredPhoto, error) {
	var ph StoredPhoto
	err := s.db.QueryRowContext(ctx, `
		SELECT ph.jpeg, po.state
		FROM photos ph
		JOIN items i ON i.id = ph.item_id
		JOIN polls po ON po.id = i.poll_id
		WHERE ph.id = ?`, photoID,
	).Scan(&ph.JPEG, &ph.PollState)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredPhoto{}, ErrNotFound
	}
	if err != nil {
		return StoredPhoto{}, fmt.Errorf("read photo: %w", err)
	}
	return ph, nil
}

// SaveRatings applies one voter's changes to a poll in one transaction. A score
// of 0 clears the rating, and items not in scores are left as they are. It
// returns ErrConflict if the poll isn't open, which also covers a poll closing
// while the voter's form was on screen.
func (s *Store) SaveRatings(ctx context.Context, pollID string, voterHash []byte, scores map[string]int) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var state State
		err := tx.QueryRowContext(ctx, `SELECT state FROM polls WHERE id = ?`, pollID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != StateOpen {
			return ErrConflict
		}
		for itemID, score := range scores {
			if score < 0 || score > 5 {
				return fmt.Errorf("score out of range")
			}
			var exists bool
			err := tx.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM items WHERE id = ? AND poll_id = ?)`, itemID, pollID,
			).Scan(&exists)
			if err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
			if score == 0 {
				_, err = tx.ExecContext(ctx, `DELETE FROM ratings WHERE voter_hash = ? AND item_id = ?`, voterHash, itemID)
			} else {
				_, err = tx.ExecContext(ctx, `
					INSERT INTO ratings (voter_hash, item_id, score) VALUES (?, ?, ?)
					ON CONFLICT (voter_hash, item_id) DO UPDATE SET score = excluded.score`,
					voterHash, itemID, score)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("save ratings: %w", err)
	}
	return nil
}

// VoterRatings returns one voter's own ratings in a poll, by item ID.
func (s *Store) VoterRatings(ctx context.Context, pollID string, voterHash []byte) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.item_id, r.score
		FROM ratings r JOIN items i ON i.id = r.item_id
		WHERE i.poll_id = ? AND r.voter_hash = ?`, pollID, voterHash)
	if err != nil {
		return nil, fmt.Errorf("read ratings: %w", err)
	}
	defer rows.Close()
	scores := make(map[string]int)
	for rows.Next() {
		var itemID string
		var score int
		if err := rows.Scan(&itemID, &score); err != nil {
			return nil, fmt.Errorf("read rating: %w", err)
		}
		scores[itemID] = score
	}
	return scores, rows.Err()
}

// Totals returns each rated item's average and count, plus how many distinct
// people rated anything in the poll.
func (s *Store) Totals(ctx context.Context, pollID string) (map[string]Total, int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.item_id, COUNT(*), AVG(r.score)
		FROM ratings r JOIN items i ON i.id = r.item_id
		WHERE i.poll_id = ?
		GROUP BY r.item_id`, pollID)
	if err != nil {
		return nil, 0, fmt.Errorf("read totals: %w", err)
	}
	defer rows.Close()
	totals := make(map[string]Total)
	for rows.Next() {
		var itemID string
		var t Total
		if err := rows.Scan(&itemID, &t.Count, &t.Average); err != nil {
			return nil, 0, fmt.Errorf("read total: %w", err)
		}
		totals[itemID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rows.Close() // release the single connection before the next query

	var voters int
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT r.voter_hash)
		FROM ratings r JOIN items i ON i.id = r.item_id
		WHERE i.poll_id = ?`, pollID).Scan(&voters)
	if err != nil {
		return nil, 0, fmt.Errorf("count voters: %w", err)
	}
	return totals, voters, nil
}
