package bazaraki_test

import (
	"os"
	"strings"
	"testing"

	"github.com/webbash/scrapper-bot/internal/bazaraki"
)

func fixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name + ".html")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestParseSearch(t *testing.T) {
	page, err := bazaraki.ParseSearch(fixture(t, "search"))
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Listings) != 2 {
		t.Fatalf("want two unique results (no recommendations), got %+v", page.Listings)
	}
	first, second := page.Listings[0], page.Listings[1]
	if first.ID != 1000001 || second.ID != 1000002 {
		t.Fatalf("unexpected result order: %d, %d", first.ID, second.ID)
	}
	if first.URL != "https://www.bazaraki.com/adv/1000001_sample-apartment/" ||
		first.Title != "Sample apartment" || first.Location.Name != "Limassol — Sample area" ||
		first.PriceEURCents == nil || *first.PriceEURCents != 175050 {
		t.Fatalf("unexpected first result: %+v", first)
	}
	if first.Location.Coordinates != nil || first.ConstructionYear != nil || first.Furnishing != "" {
		t.Fatalf("search filters and map center do not describe an ad: %+v", first)
	}
	if second.PriceEURCents != nil {
		t.Fatalf("unexpected EUR price for a non-EUR result: %+v", second)
	}
}

func TestParseListingWithCoordinatesAndMultilineDescription(t *testing.T) {
	ad, err := bazaraki.ParseListing(fixture(t, "listing"))
	if err != nil {
		t.Fatal(err)
	}
	if ad.ID != 1000001 || ad.Title != "Sample apartment" || ad.PublishedLabel != "Today" ||
		ad.Location.Name != "Limassol — Sample area" ||
		ad.PriceEURCents == nil || *ad.PriceEURCents != 175000 ||
		ad.ConstructionYear == nil || *ad.ConstructionYear != 2020 ||
		ad.Bedrooms == nil || *ad.Bedrooms != 2 || ad.Furnishing != "appliances_only" {
		t.Fatalf("unexpected ad: %+v", ad)
	}
	if ad.Description != "Техника: холодильник.\nDishwasher & dryer." {
		t.Fatalf("unexpected description: %q", ad.Description)
	}
	p := ad.Location.Coordinates
	if p == nil || p.Latitude != 34.67 || p.Longitude != 33.04 || p.Approximate == nil || *p.Approximate {
		t.Fatalf("expected the canonical ad's non-approximate map point, got %+v", p)
	}
}

func TestParseListingOptionalLocationPrecision(t *testing.T) {
	for _, tt := range []struct {
		name      string
		html      string
		wantPoint bool
		approx    *bool
	}{
		{"area without coordinates", "null", false, nil},
		{"approximate point", `{"lat":34.67,"lng":33.04,"is_approximate":true}`, true, boolPtr(true)},
		{"point without a precision label", `{"lat":34.67,"lng":33.04}`, true, nil},
		{"missing longitude", `{"lat":34.67}`, false, nil},
		{"invalid latitude", `{"lat":91,"lng":33.04,"is_approximate":false}`, false, nil},
		{"zero placeholder", `{"lat":0,"lng":0,"is_approximate":false}`, false, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source, err := os.ReadFile("testdata/listing.html")
			if err != nil {
				t.Fatal(err)
			}
			doc := strings.Replace(string(source), `{\"lat\":34.67,\"lng\":33.04,\"is_approximate\":false}`, strings.ReplaceAll(tt.html, `"`, `\"`), 1)
			ad, err := bazaraki.ParseListing(strings.NewReader(doc))
			if err != nil {
				t.Fatal(err)
			}
			if ad.Location.Name != "Limassol — Sample area" {
				t.Fatalf("area was lost: %+v", ad.Location)
			}
			got := ad.Location.Coordinates
			if !tt.wantPoint && got != nil || tt.wantPoint && got == nil {
				t.Fatalf("coordinates = %+v, want point: %v", got, tt.wantPoint)
			}
			if got != nil && ((tt.approx == nil) != (got.Approximate == nil) ||
				tt.approx != nil && *got.Approximate != *tt.approx) {
				t.Fatalf("unexpected precision flag in %+v", got)
			}
		})
	}
}

func TestParseSearchDistinguishesEmptyFromUnrecognized(t *testing.T) {
	page, err := bazaraki.ParseSearch(fixture(t, "search-empty"))
	if err != nil || len(page.Listings) != 0 {
		t.Fatalf("expected a recognized empty page, got %+v, %v", page, err)
	}
	if _, err := bazaraki.ParseSearch(strings.NewReader("<html><h1>Not a results page</h1></html>")); err == nil {
		t.Fatal("unrecognized page was reported as a successful empty search")
	}
	if _, err := bazaraki.ParseListing(fixture(t, "search")); err == nil {
		t.Fatal("search page was accepted as a listing")
	}
}

func boolPtr(value bool) *bool { return &value }

func TestUnexpectedGalleryDoesNotLoseListingOrCoordinates(t *testing.T) {
	raw, err := os.ReadFile("testdata/listing.html")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(raw), `\"small\":[\"https://cdn1.bazaraki.com/sample/photo.webp\"]`, `\"small\":42`, 1)
	if source == string(raw) {
		t.Fatal("fixture gallery did not match")
	}
	ad, err := bazaraki.ParseListing(strings.NewReader(source))
	if err != nil || ad.ID != 1000001 || ad.Location.Coordinates == nil || ad.PhotoURL != "" {
		t.Fatalf("unexpected listing: %+v, %v", ad, err)
	}
}
