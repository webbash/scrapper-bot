package bazaraki_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/webbash/scrapper-bot/internal/bazaraki"
)

const (
	searchURL  = "https://www.bazaraki.com/real-estate-to-rent/apartments-flats/lemesos-district-limassol/"
	listingURL = "https://www.bazaraki.com/adv/1000001_sample-apartment/"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(status int, contentType string, body io.Reader, size int64) *http.Response {
	headers := make(http.Header)
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(body), ContentLength: size}
}

func TestFetchHTMLPassesSnapshotsToTheParser(t *testing.T) {
	for _, tt := range []struct {
		kind string
		url  string
	}{
		{"search", searchURL + "?price_max=1750&sort=newest"},
		{"listing", listingURL},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			snapshot, err := os.ReadFile("testdata/" + tt.kind + ".html")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			f := bazaraki.Fetcher{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodGet || req.URL.String() != tt.url {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
				}
				if req.Header.Get("Accept-Language") != "en" || req.Header.Get("Cookie") != "" {
					t.Fatalf("unexpected English/session headers: %v", req.Header)
				}
				if _, ok := req.Context().Deadline(); !ok {
					t.Fatal("request has no deadline")
				}
				return response(http.StatusOK, "text/html; charset=UTF-8", bytes.NewReader(snapshot), int64(len(snapshot))), nil
			})}

			body, err := f.FetchHTML(t.Context(), tt.url+"#local-anchor")
			if err != nil || !bytes.Equal(body, snapshot) || calls != 1 {
				t.Fatalf("fetch returned %d bytes (%v) with %d requests", len(body), err, calls)
			}
			if tt.kind == "search" {
				page, err := bazaraki.ParseSearch(bytes.NewReader(body))
				if err != nil || len(page.Listings) != 2 {
					t.Fatalf("fetched search did not parse: %+v, %v", page, err)
				}
			} else {
				ad, err := bazaraki.ParseListing(bytes.NewReader(body))
				if err != nil || ad.ID != 1000001 || ad.Location.Coordinates == nil {
					t.Fatalf("fetched listing did not parse: %+v, %v", ad, err)
				}
			}
		})
	}
}

func TestFetchHTMLPreservesNativePolygonAndSearchFilters(t *testing.T) {
	const filterPath = "/real-estate-to-rent/apartments-flats/construction-year_min---2018/number-of-bedrooms---1/number-of-bedrooms---2/type---5/"
	// Synthetic value with characters percent-encoded in links copied from the map.
	const query = "polygon=sample%40a%7Bb%60c%7D&ordering=newest&price_max=1800"
	f := bazaraki.Fetcher{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.EscapedPath() != filterPath || req.URL.RawQuery != query {
			t.Fatalf("filters changed: %s", req.URL.RequestURI())
		}
		return response(http.StatusOK, "text/html", strings.NewReader("<html>Filtered results</html>"), -1), nil
	})}
	if _, err := f.FetchHTML(t.Context(), "https://www.bazaraki.com"+filterPath+"?"+query); err != nil {
		t.Fatal(err)
	}
}

func TestFetchHTMLRejectsUnsupportedURLBeforeRequest(t *testing.T) {
	f := bazaraki.Fetcher{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected HTTP request: %s", req.URL)
		return nil, nil
	})}
	for _, u := range []string{
		"http://www.bazaraki.com/adv/1000001_sample-apartment/",
		"https://bazaraki.com.example.com/adv/1000001_sample-apartment/",
		"https://name@www.bazaraki.com/adv/1000001_sample-apartment/",
		"https://www.bazaraki.com:443/adv/1000001_sample-apartment/",
		"https://www.bazaraki.com/real-estate-to-rent/apartments-flats/%2e%2e/profile/",
		"https://www.bazaraki.com/profile/",
		"/adv/1000001_sample-apartment/",
	} {
		if _, err := f.FetchHTML(t.Context(), u); err == nil {
			t.Errorf("accepted unsupported URL %q", u)
		}
	}
}

func TestFetchHTMLChecksRedirectsBeforeFollowing(t *testing.T) {
	for _, tt := range []struct {
		name      string
		location  string
		loop      bool
		wantCalls int
		wantError string
	}{
		{"Bazaraki relative redirect", "/adv/1000001_sample-apartment/", false, 2, ""},
		{"external redirect", "https://example.com/adv/1000001_sample-apartment/", false, 1, "HTTPS URL on bazaraki.com"},
		{"same origin login redirect", "/profile/login/", false, 1, "apartment rental search"},
		{"redirect loop", searchURL, true, 4, "too many Bazaraki redirects"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			f := bazaraki.Fetcher{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 && !tt.loop {
					if req.URL.String() != listingURL {
						t.Fatalf("followed unexpected redirect: %s", req.URL)
					}
					return response(http.StatusOK, "text/html", strings.NewReader("<html>ad</html>"), -1), nil
				}
				r := response(http.StatusFound, "text/html", strings.NewReader(""), 0)
				r.Header.Set("Location", tt.location)
				return r, nil
			})}
			_, err := f.FetchHTML(t.Context(), searchURL)
			if calls != tt.wantCalls || !errorContains(err, tt.wantError) {
				t.Fatalf("requests: %d, error: %v; want %d and %q", calls, err, tt.wantCalls, tt.wantError)
			}
		})
	}
}

func TestFetchHTMLRejectsInvalidResponsesWithoutRetry(t *testing.T) {
	const tooLarge = 8<<20 + 1
	for _, tt := range []struct {
		name        string
		status      int
		contentType string
		size        int64
		body        string
		wantError   string
	}{
		{"blocked", http.StatusForbidden, "text/html", -1, "<html>Blocked</html>", "HTTP 403"},
		{"rate limited", http.StatusTooManyRequests, "text/html", -1, "", "HTTP 429"},
		{"missing listing", http.StatusNotFound, "text/html", -1, "", "HTTP 404"},
		{"JSON", http.StatusOK, "application/json", -1, "{}", "expected Bazaraki HTML"},
		{"empty", http.StatusOK, "text/html", -1, "", "empty HTML"},
		{"too large with length", http.StatusOK, "text/html", tooLarge, "", "exceeds"},
		{"too large without length", http.StatusOK, "text/html", -1, strings.Repeat("x", tooLarge), "exceeds"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			f := bazaraki.Fetcher{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return response(tt.status, tt.contentType, strings.NewReader(tt.body), tt.size), nil
			})}
			body, err := f.FetchHTML(t.Context(), searchURL)
			if body != nil || calls != 1 || !errorContains(err, tt.wantError) {
				t.Fatalf("body: %d bytes, requests: %d, error: %v", len(body), calls, err)
			}
		})
	}
}

func TestFetchHTMLHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := bazaraki.Fetcher{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})}
	if _, err := f.FetchHTML(ctx, searchURL); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func errorContains(err error, want string) bool {
	if want == "" {
		return err == nil
	}
	return err != nil && strings.Contains(err.Error(), want)
}
