package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseSearchFromStdin(t *testing.T) {
	snapshot, err := os.Open("../../internal/bazaraki/testdata/search.html")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	var output bytes.Buffer
	if err := run(t.Context(), []string{"search", "-"}, snapshot, &output, nil); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Listings []struct {
			ID int64
		}
	}
	if err := json.Unmarshal(output.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Listings) != 2 || page.Listings[0].ID != 1000001 {
		t.Fatalf("unexpected results: %+v", page.Listings)
	}
}

func TestFetchPrintsParsedJSONInOneCommand(t *testing.T) {
	for _, tt := range []struct {
		kind string
		url  string
	}{
		{"search", "https://www.bazaraki.com/real-estate-to-rent/apartments-flats/lemesos-district-limassol/"},
		{"listing", "https://www.bazaraki.com/adv/1000001_sample-apartment/"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			input, err := os.ReadFile("../../internal/bazaraki/testdata/" + tt.kind + ".html")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			fetch := func(_ context.Context, u string) ([]byte, error) {
				calls++
				if u != tt.url {
					t.Fatalf("requested %q, want %q", u, tt.url)
				}
				return input, nil
			}

			var out bytes.Buffer
			if err := run(t.Context(), []string{"fetch", tt.url}, nil, &out, fetch); err != nil {
				t.Fatal(err)
			}
			var page struct {
				Listings []struct {
					ID int64
				}
				ID       int64
				Location struct {
					Coordinates *struct {
						Latitude float64
					}
				}
			}
			if err := json.Unmarshal(out.Bytes(), &page); err != nil {
				t.Fatalf("fetch did not emit JSON: %v", err)
			}
			switch tt.kind {
			case "search":
				if len(page.Listings) != 2 || page.Listings[0].ID != 1000001 || page.ID != 0 {
					t.Fatalf("unexpected search data: %s", out.Bytes())
				}
			case "listing":
				if page.ID != 1000001 || page.Location.Coordinates == nil || page.Location.Coordinates.Latitude != 34.67 {
					t.Fatalf("unexpected listing data: %s", out.Bytes())
				}
			}
			if calls != 1 {
				t.Fatalf("made %d fetches, want one", calls)
			}
		})
	}
}

func TestFetchErrorsDoNotWriteHTMLOrJSON(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		err  error
		want string
	}{
		{"network error", "", errors.New("HTTP 403"), "HTTP 403"},
		{"non-listing HTML", "<html>Verification page</html>", nil, "parse fetched Bazaraki page"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(t.Context(), []string{"fetch", "test-url"}, nil, &out, func(context.Context, string) ([]byte, error) {
				return []byte(tt.body), tt.err
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) || out.Len() != 0 {
				t.Fatalf("error = %v, stdout = %q", err, out.String())
			}
		})
	}
}

func TestFetchHTMLPreservesRawSnapshot(t *testing.T) {
	const html = "<html>Source only</html>\n"
	var out bytes.Buffer
	err := run(t.Context(), []string{"fetch-html", "test-url"}, nil, &out, func(context.Context, string) ([]byte, error) {
		return []byte(html), nil
	})
	if err != nil || out.String() != html {
		t.Fatalf("error = %v, stdout = %q", err, out.String())
	}
}
