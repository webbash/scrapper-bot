package housing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/webbash/scrapper-bot/internal/bazaraki"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS housing_chats (
  chat_id INTEGER PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS housing_listings (
  chat_id INTEGER NOT NULL,
  listing_id INTEGER NOT NULL,
  payload TEXT NOT NULL,
  delivery TEXT NOT NULL CHECK (delivery IN ('baseline', 'pending', 'sending', 'sent')),
  message_id INTEGER,
  sent_at INTEGER,
  decision TEXT CHECK (decision IN ('suitable', 'unsuitable')),
  decision_at INTEGER,
  PRIMARY KEY (chat_id, listing_id)
);
CREATE INDEX IF NOT EXISTS housing_favorites ON housing_listings (chat_id, decision, decision_at DESC, listing_id DESC);
`

func Open(ctx context.Context, filename string) (*Store, error) {
	abs, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("resolve SQLite filename: %w", err)
	}
	file, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("prepare SQLite file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("prepare SQLite file: %w", err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	u.RawQuery = url.Values{"_pragma": {"busy_timeout(5000)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize SQLite: %w", err)
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RecordSearch marks the first successful search as an atomic baseline. Existing
// IDs are left alone on subsequent runs, including IDs already rated or sent.
func (s *Store) RecordSearch(ctx context.Context, chatID int64, ads []bazaraki.Listing) (baseline bool, added int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	executed, err := tx.ExecContext(ctx, "INSERT INTO housing_chats(chat_id) VALUES (?) ON CONFLICT DO NOTHING", chatID)
	fresh, err := affected(executed, err)
	if err != nil {
		return false, 0, err
	}
	delivery := "pending"
	if fresh {
		delivery = "baseline"
	}
	for _, ad := range ads {
		data, err := json.Marshal(ad)
		if err != nil {
			return false, 0, err
		}
		executed, err := tx.ExecContext(ctx, `
			INSERT INTO housing_listings(chat_id, listing_id, payload, delivery)
			VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, chatID, ad.ID, string(data), delivery)
		inserted, err := affected(executed, err)
		if err != nil {
			return false, 0, err
		}
		if inserted {
			added++
		}
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return fresh, added, nil
}

func (s *Store) pending(ctx context.Context, chatID, id int64) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM housing_listings WHERE chat_id = ? AND listing_id = ? AND delivery = 'pending')
	`, chatID, id).Scan(&pending)
	return pending, err
}

// saveTest records a test card independently of the first search baseline.
func (s *Store) saveTest(ctx context.Context, chatID int64, ad bazaraki.Listing) error {
	data, err := json.Marshal(ad)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO housing_listings(chat_id, listing_id, payload, delivery)
		VALUES (?, ?, ?, 'pending') ON CONFLICT DO NOTHING
	`, chatID, ad.ID, string(data))
	return err
}

func (s *Store) claim(ctx context.Context, chatID int64, ad bazaraki.Listing) (bool, error) {
	data, err := json.Marshal(ad)
	if err != nil {
		return false, err
	}
	return affected(s.db.ExecContext(ctx, `
		UPDATE housing_listings SET delivery = 'sending', payload = ?
		WHERE chat_id = ? AND listing_id = ? AND delivery = 'pending'
	`, string(data), chatID, ad.ID))
}

func (s *Store) sent(ctx context.Context, chatID, id, messageID int64) error {
	recorded, err := affected(s.db.ExecContext(ctx, `
		UPDATE housing_listings SET delivery = 'sent', sent_at = ?, message_id = ?
		WHERE chat_id = ? AND listing_id = ? AND delivery = 'sending'
	`, time.Now().UnixMilli(), messageID, chatID, id))
	if err == nil && !recorded {
		return fmt.Errorf("no claimed alert to complete for %d", id)
	}
	return err
}

func (s *Store) release(ctx context.Context, chatID, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE housing_listings SET delivery = 'pending'
		WHERE chat_id = ? AND listing_id = ? AND delivery = 'sending' AND decision IS NULL
	`, chatID, id)
	return err
}

func (s *Store) HeldCount(ctx context.Context, chatID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM housing_listings WHERE chat_id = ? AND delivery = 'sending'", chatID).Scan(&n)
	return n, err
}

type Decision string

const (
	Suitable   Decision = "suitable"
	Unsuitable Decision = "unsuitable"
)

func (s *Store) Decide(ctx context.Context, chatID, id, messageID int64, decision Decision) (bool, error) {
	if messageID <= 0 {
		return false, nil
	}
	return affected(s.db.ExecContext(ctx, `
		UPDATE housing_listings SET decision = ?,
			decision_at = CASE WHEN decision IS ? THEN decision_at ELSE ? END
		WHERE chat_id = ? AND listing_id = ? AND message_id = ? AND delivery = 'sent'
	`, decision, decision, time.Now().UnixMilli(), chatID, id, messageID))
}

func (s *Store) Favorites(ctx context.Context, chatID int64, offset, limit int) ([]bazaraki.Listing, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload FROM housing_listings WHERE chat_id = ? AND decision = 'suitable'
		ORDER BY decision_at DESC, listing_id DESC LIMIT ? OFFSET ?
	`, chatID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ads []bazaraki.Listing
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var ad bazaraki.Listing
		if err := json.Unmarshal(data, &ad); err != nil {
			return nil, err
		}
		ads = append(ads, ad)
	}
	return ads, rows.Err()
}

func affected(r sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}
