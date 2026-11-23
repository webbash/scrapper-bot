package housing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webbash/scrapper-bot/internal/bazaraki"
	"github.com/webbash/scrapper-bot/internal/telegram"
)

const groupID int64 = -100000001
const samplePhoto = "https://cdn1.bazaraki.com/sample/photo.jpg"

type sentMessage struct {
	where    int64
	text     string
	photo    string
	keyboard *telegram.Keyboard
}

type telegramStub struct {
	texts, photos  []sentMessage
	answers        []string
	photoErr       error
	deleteErr      error
	deleted        []int64
	collapsed      []int64
	collapsedPhoto bool
	updates        func(context.Context, int64) ([]telegram.Update, error)
}

func (s *telegramStub) SendText(_ context.Context, chat int64, text, _ string, keyboard *telegram.Keyboard) (telegram.Message, error) {
	s.texts = append(s.texts, sentMessage{chat, text, "", keyboard})
	return telegram.Message{ID: 23}, nil
}

func (s *telegramStub) SendPhoto(_ context.Context, chat int64, photo, text string, keyboard *telegram.Keyboard) (telegram.Message, error) {
	s.photos = append(s.photos, sentMessage{chat, text, photo, keyboard})
	return telegram.Message{ID: int64(22 + len(s.photos))}, s.photoErr
}

func (s *telegramStub) Answer(_ context.Context, _, text string) error {
	s.answers = append(s.answers, text)
	return nil
}

func (s *telegramStub) DeleteMessage(_ context.Context, _, id int64) error {
	s.deleted = append(s.deleted, id)
	return s.deleteErr
}

func (s *telegramStub) CollapseMessage(_ context.Context, _, id int64, photo bool) error {
	s.collapsed = append(s.collapsed, id)
	s.collapsedPhoto = photo
	return nil
}

func (s *telegramStub) Updates(ctx context.Context, offset int64) ([]telegram.Update, error) {
	if s.updates != nil {
		return s.updates(ctx, offset)
	}
	return nil, errors.New("not polling during test")
}

func TestBotRunReturnsFatalTelegramError(t *testing.T) {
	unauthorized := &telegram.APIError{Code: 401, Description: "Unauthorized"}
	tg := &telegramStub{updates: func(context.Context, int64) ([]telegram.Update, error) {
		return nil, unauthorized
	}}
	b := Bot{API: tg}
	if err := b.Run(t.Context(), io.Discard); !errors.Is(err, unauthorized) {
		t.Fatalf("bot did not exit with the fatal Telegram error: %v", err)
	}
}

func sample(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "bazaraki", "testdata", name+".html"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func openDB(t *testing.T, filename string) *Store {
	t.Helper()
	s, err := Open(t.Context(), filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestManualSyncThenSharedTelegramDecisions(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "shared.sqlite")
	db := openDB(t, filename)
	search := sample(t, "search")
	detail := sample(t, "listing")
	initial := strings.NewReplacer("1000001", "2000001", "1000002", "2000002").Replace(search)
	searchURL := "https://www.bazaraki.com/real-estate-to-rent/apartments-flats/lemesos-district-limassol/"
	requests := 0
	fetch := func(_ context.Context, addr string) ([]byte, error) {
		requests++
		switch {
		case addr == searchURL:
			return []byte(initial), nil
		case strings.Contains(addr, "/adv/1000001_"):
			return []byte(detail), nil
		case strings.Contains(addr, "/adv/1000002_"):
			return []byte(strings.ReplaceAll(detail, "1000001", "1000002")), nil
		default:
			return nil, errors.New("unexpected URL: " + addr)
		}
	}
	tg := &telegramStub{}
	bot := Bot{groupID, db, tg}
	first, err := Sync(ctx, db, groupID, searchURL, fetch, bot.SendListing)
	if err != nil || !first.Baseline || requests != 1 || first.New != 2 || len(tg.photos)+len(tg.texts) != 0 {
		t.Fatalf("baseline: %+v, requests=%d, error=%v", first, requests, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Bot and manual sync can use the same persistent file with different connections.
	db = openDB(t, filename)
	bot.Store = openDB(t, filename)
	initial = search
	result, err := Sync(ctx, db, groupID, searchURL, fetch, bot.SendListing)
	if err != nil || result.Baseline || result.New != 2 || result.Sent != 2 || requests != 4 || len(tg.photos) != 2 {
		t.Fatalf("second sync: %+v, requests=%d, error=%v", result, requests, err)
	}
	photo := tg.photos[0]
	keys := photo.keyboard.Rows
	if photo.where != groupID || photo.photo != samplePhoto || !strings.Contains(photo.text, "34.670000,33.040000") ||
		len(keys) != 3 || keys[0][0].Callback != "yes:1000001" || keys[0][1].Callback != "no:1000001" ||
		keys[1][1].Callback != "favorites:0" || keys[2][0].URL == "" {
		t.Fatalf("unexpected Telegram photo: %+v, buttons=%+v", photo, keys)
	}

	for _, click := range []struct {
		chat   int64
		action string
		msg    int64
	}{
		{groupID, "yes:1000001", 23},
		{groupID, "no:1000002", 24},
		{groupID + 1, "no:1000001", 23}, // A different chat must not change the shared Favorites.
		{groupID, "favorites:0", 23},
	} {
		m := &telegram.Message{ID: click.msg}
		m.Chat.ID = click.chat
		u := telegram.Update{Callback: &telegram.Callback{ID: "callback", Data: click.action, Message: m}}
		if err := bot.Handle(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if len(tg.deleted) != 1 || tg.deleted[0] != 24 || len(tg.collapsed) != 0 {
		t.Fatalf("the bot deleted the wrong messages: %v", tg.deleted)
	}
	if len(tg.answers) != 3 || len(tg.texts) != 1 || !strings.Contains(tg.texts[0].text, "1000001_sample") ||
		strings.Contains(tg.texts[0].text, "1000002_sample") {
		t.Fatalf("unexpected Favorites: texts=%+v, answers=%+v", tg.texts, tg.answers)
	}
	// Publishing label and price edits on the same ID must not reopen or re-alert an ad.
	initial = strings.ReplaceAll(search, "Today", "Now")
	repeated, err := Sync(ctx, db, groupID, searchURL, fetch, bot.SendListing)
	if err != nil || repeated.New != 0 || repeated.Sent != 0 || requests != 5 || len(tg.photos) != 2 {
		t.Fatalf("rerun: %+v, requests=%d, error=%v", repeated, requests, err)
	}
}

func TestUncertainSendIsHeldButExplicitRejectionCanRetry(t *testing.T) {
	for _, tt := range []struct {
		name     string
		firstErr error
		held     int
		nextSent int
	}{
		{"ambiguous timeout", context.DeadlineExceeded, 1, 1},
		{"explicit rejection", &telegram.APIError{Code: 429}, 0, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			filename := filepath.Join(t.TempDir(), "state.sqlite")
			db := openDB(t, filename)
			if _, _, err := db.RecordSearch(ctx, groupID, nil); err != nil {
				t.Fatal(err)
			}
			search, detail := sample(t, "search"), sample(t, "listing")
			fetch := func(_ context.Context, addr string) ([]byte, error) {
				if addr == "search" {
					return []byte(search), nil
				}
				if strings.Contains(addr, "1000002_") {
					return []byte(strings.ReplaceAll(detail, "1000001", "1000002")), nil
				}
				return []byte(detail), nil
			}
			once := func(context.Context, int64, bazaraki.Listing) (int64, error) {
				return 0, tt.firstErr
			}
			if _, err := Sync(ctx, db, groupID, "search", fetch, once); err == nil {
				t.Fatal("expected send error")
			}
			held, err := db.HeldCount(ctx, groupID)
			if err != nil || held != tt.held {
				t.Fatalf("held=%d, want %d, err=%v", held, tt.held, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = openDB(t, filename)
			again, err := Sync(ctx, db, groupID, "search", fetch, func(context.Context, int64, bazaraki.Listing) (int64, error) {
				return 25, nil
			})
			if err != nil || again.New != 0 || again.Sent != tt.nextSent {
				t.Fatalf("retry: %+v, err=%v", again, err)
			}
		})
	}
}

func TestTelegramPhotoFallbackAndMissingPoint(t *testing.T) {
	ad := bazaraki.Listing{ID: 100, URL: "https://www.bazaraki.com/adv/100_sample/", Title: "Flat", PhotoURL: samplePhoto}
	tg := &telegramStub{photoErr: &telegram.APIError{Code: 400, Description: "wrong file identifier/HTTP URL specified"}}
	b := Bot{ChatID: groupID, API: tg}
	if _, err := b.SendListing(t.Context(), groupID, ad); err != nil || len(tg.photos) != 1 ||
		len(tg.texts) != 1 || len(tg.texts[0].keyboard.Rows) != 2 {
		t.Fatalf("expected text fallback without fabricated point: %+v, %v", tg, err)
	}
	tg.photoErr = context.DeadlineExceeded
	if _, err := b.SendListing(t.Context(), groupID, ad); !errors.Is(err, context.DeadlineExceeded) || len(tg.texts) != 1 {
		t.Fatalf("ambiguous photo error must not send a second message: %+v, %v", tg, err)
	}
}

func TestOneTestCardSharesButtonsButDoesNotAlterSearchBaseline(t *testing.T) {
	ctx := t.Context()
	db := openDB(t, filepath.Join(t.TempDir(), "smoke.sqlite"))
	ad, err := bazaraki.ParseListing(strings.NewReader(sample(t, "listing")))
	if err != nil {
		t.Fatal(err)
	}
	tg := &telegramStub{}
	bot := Bot{ChatID: groupID, Store: db, API: tg}
	for attempt := range 2 {
		sent, err := SendTestCard(ctx, db, groupID, ad, bot.SendListing)
		if err != nil || sent != (attempt == 0) {
			t.Fatalf("attempt %d: sent=%v, error=%v", attempt, sent, err)
		}
	}
	if len(tg.photos) != 1 || !strings.Contains(tg.photos[0].text, "ТЕСТ · ") ||
		tg.photos[0].photo != samplePhoto || len(tg.photos[0].keyboard.Rows) != 3 {
		t.Fatalf("unexpected test alert: %+v", tg.photos)
	}
	m := &telegram.Message{ID: 23}
	m.Chat.ID = groupID
	for _, callback := range []string{"yes:-1000001", "favorites:0", "no:-1000001", "favorites:0"} {
		if err := bot.Handle(ctx, telegram.Update{Callback: &telegram.Callback{ID: "test", Data: callback, Message: m}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(tg.texts) != 2 || !strings.Contains(tg.texts[0].text, "ТЕСТ · ") || !strings.Contains(tg.texts[1].text, "пока нет") {
		t.Fatalf("unexpected Favorites: %+v", tg.texts)
	}
	baseline, added, err := db.RecordSearch(ctx, groupID, []bazaraki.Listing{ad})
	if err != nil || !baseline || added != 1 {
		t.Fatalf("test card changed the normal baseline: %v, %d, %v", baseline, added, err)
	}
}

func TestRejectingOldCardUsesOnlyItsPersistedMessage(t *testing.T) {
	ctx := t.Context()
	db := openDB(t, filepath.Join(t.TempDir(), "test.sqlite"))
	ad, err := bazaraki.ParseListing(strings.NewReader(sample(t, "listing")))
	if err != nil {
		t.Fatal(err)
	}
	tg := &telegramStub{deleteErr: &telegram.APIError{Code: 400, Description: "message can't be deleted"}}
	bot := Bot{ChatID: groupID, Store: db, API: tg}
	if sent, err := SendTestCard(ctx, db, groupID, ad, bot.SendListing); err != nil || !sent {
		t.Fatalf("send test card: sent=%v, error=%v", sent, err)
	}
	for _, id := range []int64{999, 23} {
		msg := telegram.Message{ID: id, Photo: []json.RawMessage{json.RawMessage("{}")}}
		msg.Chat.ID = groupID
		u := telegram.Update{Callback: &telegram.Callback{ID: "click", Data: "no:-1000001", Message: &msg}}
		if err := bot.Handle(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if len(tg.deleted) != 1 || tg.deleted[0] != 23 || len(tg.collapsed) != 1 || tg.collapsed[0] != 23 || !tg.collapsedPhoto {
		t.Fatalf("wrong Telegram effects: deleted=%v, collapsed=%v, photo=%v", tg.deleted, tg.collapsed, tg.collapsedPhoto)
	}
}

func TestOnlyOneSQLiteHandleCanClaimTheSameAlert(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "shared.sqlite")
	a, b := openDB(t, filename), openDB(t, filename)
	if _, _, err := a.RecordSearch(t.Context(), groupID, nil); err != nil {
		t.Fatal(err)
	}
	ad := bazaraki.Listing{ID: 10}
	if _, _, err := a.RecordSearch(t.Context(), groupID, []bazaraki.Listing{ad}); err != nil {
		t.Fatal(err)
	}
	type result struct {
		claimed bool
		err     error
	}
	results := make(chan result, 2)
	for _, db := range []*Store{a, b} {
		go func() {
			ok, err := db.claim(t.Context(), groupID, ad)
			results <- result{ok, err}
		}()
	}
	claims := 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.claimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("got %d deliveries claimed, want 1", claims)
	}
}

func TestUnavailableListingDoesNotBlockOtherNewAds(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "state.sqlite"))
	if _, _, err := db.RecordSearch(t.Context(), groupID, nil); err != nil {
		t.Fatal(err)
	}
	search, detail := sample(t, "search"), sample(t, "listing")
	fetch := func(_ context.Context, addr string) ([]byte, error) {
		if addr == "search" {
			return []byte(search), nil
		}
		if strings.Contains(addr, "1000001_") {
			return nil, &bazaraki.HTTPStatusError{StatusCode: 404}
		}
		return []byte(strings.ReplaceAll(detail, "1000001", "1000002")), nil
	}
	report, err := Sync(t.Context(), db, groupID, "search", fetch, func(_ context.Context, _ int64, ad bazaraki.Listing) (int64, error) {
		if ad.ID != 1000002 {
			t.Fatalf("did not skip unavailable listing: %d", ad.ID)
		}
		return 27, nil
	})
	if err != nil || report.Unavailable != 1 || report.Sent != 1 {
		t.Fatalf("sync: %+v, err=%v", report, err)
	}
}
