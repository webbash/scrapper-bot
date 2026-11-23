package housing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/webbash/scrapper-bot/internal/bazaraki"
)

// FetchHTML and SendListing are explicit seams: one invocation has no timer
// and tests can supply saved HTML and a fake Telegram transport.
type FetchHTML func(context.Context, string) ([]byte, error)
type SendListing func(context.Context, int64, bazaraki.Listing) (int64, error)

type Report struct {
	Results     int
	New         int
	Sent        int
	Unavailable int
	Baseline    bool
}

func Sync(ctx context.Context, store *Store, chatID int64, searchURL string, fetch FetchHTML, send SendListing) (report Report, err error) {
	html, err := fetch(ctx, searchURL)
	if err != nil {
		return report, fmt.Errorf("fetch search: %w", err)
	}
	page, err := bazaraki.ParseSearch(bytes.NewReader(html))
	if err != nil {
		return report, fmt.Errorf("parse search: %w", err)
	}
	report.Results = len(page.Listings)
	report.Baseline, report.New, err = store.RecordSearch(ctx, chatID, page.Listings)
	if err != nil || report.Baseline {
		return report, err
	}
	for _, summary := range page.Listings {
		pending, err := store.pending(ctx, chatID, summary.ID)
		if err != nil {
			return report, err
		}
		if !pending {
			continue
		}

		html, err := fetch(ctx, summary.URL)
		if err != nil {
			var status *bazaraki.HTTPStatusError
			if errors.As(err, &status) && (status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusGone) {
				report.Unavailable++
				continue
			}
			return report, fmt.Errorf("fetch listing %d (still pending): %w", summary.ID, err)
		}
		ad, err := bazaraki.ParseListing(bytes.NewReader(html))
		if err != nil {
			return report, fmt.Errorf("parse listing %d (still pending): %w", summary.ID, err)
		}
		if ad.ID != summary.ID {
			return report, fmt.Errorf("listing %d redirected to %d (still pending)", summary.ID, ad.ID)
		}

		delivered, err := sendOnce(ctx, store, chatID, ad, send)
		if err != nil {
			return report, err
		}
		if delivered {
			report.Sent++
		}
	}
	return report, nil
}

// SendTestCard uses the regular Telegram card and rating flows without marking
// a real listing as seen. Bazaraki IDs are positive, so test IDs are negative.
func SendTestCard(ctx context.Context, store *Store, chatID int64, ad bazaraki.Listing, send SendListing) (bool, error) {
	if ad.ID <= 0 || ad.PhotoURL == "" || ad.Location.Coordinates == nil {
		return false, errors.New("the test needs a saved listing with a photo and coordinates")
	}
	ad.ID = -ad.ID
	ad.Title = "ТЕСТ · " + ad.Title
	if err := store.saveTest(ctx, chatID, ad); err != nil {
		return false, err
	}
	return sendOnce(ctx, store, chatID, ad, send)
}

func sendOnce(ctx context.Context, store *Store, chatID int64, ad bazaraki.Listing, send SendListing) (bool, error) {
	claimed, err := store.claim(ctx, chatID, ad)
	if err != nil || !claimed {
		return false, err
	}
	messageID, err := send(ctx, chatID, ad)
	if err != nil {
		var rejection interface{ Rejected() bool }
		if errors.As(err, &rejection) && rejection.Rejected() {
			return false, errors.Join(fmt.Errorf("Telegram rejected listing %d: %w", ad.ID, err), store.release(ctx, chatID, ad.ID))
		}
		return false, fmt.Errorf("listing %d delivery outcome is unknown; held to avoid duplicates: %w", ad.ID, err)
	}
	if err := store.sent(ctx, chatID, ad.ID, messageID); err != nil {
		return false, fmt.Errorf("Telegram accepted listing %d but could not record it: %w", ad.ID, err)
	}
	return true, nil
}
