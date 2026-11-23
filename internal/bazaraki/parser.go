// Package bazaraki extracts rental listings from saved Bazaraki HTML.
// Parsing does not perform network requests or run JavaScript.
package bazaraki

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type SearchPage struct {
	Listings []Listing `json:"listings"`
}

type Listing struct {
	ID               int64      `json:"id"`
	URL              string     `json:"url"`
	Title            string     `json:"title"`
	PriceDisplay     string     `json:"price_display,omitempty"`
	PriceEURCents    *int64     `json:"price_eur_cents,omitempty"`
	PublishedLabel   string     `json:"published_label,omitempty"`
	Location         Location   `json:"location"`
	ConstructionYear *int       `json:"construction_year,omitempty"`
	Bedrooms         *int       `json:"bedrooms,omitempty"`
	Furnishing       Furnishing `json:"furnishing,omitempty"`
	Description      string     `json:"description,omitempty"`
	PhotoURL         string     `json:"photo_url,omitempty"`
}

type Location struct {
	Name        string       `json:"name,omitempty"`
	Coordinates *Coordinates `json:"coordinates,omitempty"`
}

type Coordinates struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// Approximate is Bazaraki's own precision marker, not proof of an address.
	// A nil value means the source did not label precision.
	Approximate *bool `json:"is_approximate,omitempty"`
}

type Furnishing string

const (
	FullyFurnished Furnishing = "fully_furnished"
	SemiFurnished  Furnishing = "semi_furnished"
	Unfurnished    Furnishing = "unfurnished"
	AppliancesOnly Furnishing = "appliances_only"
)

var (
	ErrSearchDataNotFound  = errors.New("Bazaraki search results not found in saved HTML")
	ErrListingDataNotFound = errors.New("Bazaraki listing details not found in saved HTML")
	advertPath             = regexp.MustCompile(`^/adv/([1-9][0-9]*)_[^/]+/?$`)
)

// ParsePage returns a SearchPage or Listing according to the page's canonical
// URL. The two types use the same JSON representation as the explicit parsers.
func ParsePage(r io.Reader) (any, error) {
	doc, err := readSnapshot(r)
	if err != nil {
		return nil, err
	}
	canonical := siteURL(doc.canonical)
	if canonical == nil {
		return nil, errors.New("not a recognized Bazaraki results or listing page")
	}
	if listingID(canonical) != 0 {
		return doc.parseListing(canonical)
	}
	return doc.parseSearch(canonical)
}

// ParseSearch reads the primary results, excluding recommendation carousels.
// It keeps the first occurrence of each Bazaraki listing ID.
func ParseSearch(r io.Reader) (SearchPage, error) {
	doc, err := readSnapshot(r)
	if err != nil {
		return SearchPage{}, err
	}
	return doc.parseSearch(siteURL(doc.canonical))
}

func (doc snapshot) parseSearch(canonical *url.URL) (SearchPage, error) {
	if canonical == nil || listingID(canonical) != 0 {
		return SearchPage{}, ErrSearchDataNotFound
	}
	for _, q := range doc.queries {
		target := siteURL(q.target("listing", "list"))
		if target == nil || strings.TrimRight(target.Path, "/") != strings.TrimRight(canonical.Path, "/") {
			continue
		}
		var data struct {
			Adverts []struct {
				ID           int64  `json:"id"`
				URL          string `json:"url"`
				Title        string `json:"title"`
				Price        string `json:"price"`
				NumericPrice string `json:"price_without_currency"`
				Currency     string `json:"currency"`
				Location     string `json:"location"`
				Published    string `json:"published"`
			} `json:"adverts"`
		}
		if err := json.Unmarshal(q.State.Data, &data); err != nil {
			return SearchPage{}, fmt.Errorf("decode Bazaraki search results: %w", err)
		}
		if data.Adverts == nil {
			return SearchPage{}, ErrSearchDataNotFound
		}
		page := SearchPage{Listings: make([]Listing, 0, len(data.Adverts))}
		seen := make(map[int64]bool, len(data.Adverts))
		for _, a := range data.Adverts {
			u := siteURL(a.URL)
			if a.ID <= 0 || listingID(u) != a.ID || seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			ad := Listing{
				ID: a.ID, URL: u.String(), Title: doc.text(a.Title),
				PriceDisplay: doc.text(a.Price), PublishedLabel: doc.text(a.Published),
				Location: Location{Name: doc.text(a.Location)},
			}
			if a.Currency == "EUR" {
				ad.PriceEURCents = eurCents(a.NumericPrice)
			}
			page.Listings = append(page.Listings, ad)
		}
		if len(data.Adverts) > 0 && len(page.Listings) == 0 {
			return SearchPage{}, fmt.Errorf("%w: no valid listing IDs and URLs", ErrSearchDataNotFound)
		}
		return page, nil
	}
	return SearchPage{}, ErrSearchDataNotFound
}

// ParseListing reads details tied to the ID of the canonical advertisement.
// Only coordinates recorded on that ad are returned, never a map center.
func ParseListing(r io.Reader) (Listing, error) {
	doc, err := readSnapshot(r)
	if err != nil {
		return Listing{}, err
	}
	return doc.parseListing(siteURL(doc.canonical))
}

func (doc snapshot) parseListing(canonical *url.URL) (Listing, error) {
	id := listingID(canonical)
	if id == 0 {
		return Listing{}, ErrListingDataNotFound
	}
	for _, q := range doc.queries {
		target := q.target("adverts", "detail")
		if !strings.HasPrefix(target, strconv.FormatInt(id, 10)+"_") {
			continue
		}
		var data struct {
			ID          int64           `json:"id"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Gallery     json.RawMessage `json:"gallery"`
			Price       struct {
				Display string `json:"price"`
				Numeric string `json:"price_without_currency"`
			} `json:"price"`
			Counters struct {
				Published string `json:"published"`
			} `json:"counters"`
			Location struct {
				Name   string `json:"name"`
				Coords struct {
					Lat         *float64 `json:"lat"`
					Lng         *float64 `json:"lng"`
					Approximate *bool    `json:"is_approximate"`
				} `json:"coordinates"`
			} `json:"location"`
			Features []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"features"`
		}
		if err := json.Unmarshal(q.State.Data, &data); err != nil {
			return Listing{}, fmt.Errorf("decode Bazaraki listing details: %w", err)
		}
		if data.ID != id {
			continue
		}
		ad := Listing{
			ID: id, URL: canonical.String(), Title: doc.text(data.Name),
			Description: doc.text(data.Description), PriceDisplay: doc.text(data.Price.Display),
			PublishedLabel: doc.text(data.Counters.Published),
			Location:       Location{Name: doc.text(data.Location.Name)},
			PhotoURL:       coverPhoto(data.Gallery),
		}
		if strings.HasPrefix(ad.PriceDisplay, "€") {
			ad.PriceEURCents = eurCents(data.Price.Numeric)
		}
		if p := data.Location.Coords; p.Lat != nil && p.Lng != nil {
			lat, lng := *p.Lat, *p.Lng
			if lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180 && (lat != 0 || lng != 0) {
				ad.Location.Coordinates = &Coordinates{Latitude: lat, Longitude: lng, Approximate: p.Approximate}
			}
		}
		for _, f := range data.Features {
			value := doc.text(f.Value)
			switch f.Name {
			case "Construction year":
				if year, err := strconv.Atoi(value); err == nil && year >= 1000 && year <= 9999 {
					ad.ConstructionYear = &year
				}
			case "Bedrooms":
				if bedrooms, err := strconv.Atoi(value); err == nil && bedrooms >= 0 {
					ad.Bedrooms = &bedrooms
				} else if strings.EqualFold(value, "studio") {
					studio := 0
					ad.Bedrooms = &studio
				}
			case "Furnishing":
				switch strings.ToLower(value) {
				case "fully furnished":
					ad.Furnishing = FullyFurnished
				case "semi-furnished":
					ad.Furnishing = SemiFurnished
				case "unfurnished":
					ad.Furnishing = Unfurnished
				case "appliances only", "appliances οnly":
					ad.Furnishing = AppliancesOnly
				}
			}
		}
		return ad, nil
	}
	return Listing{}, ErrListingDataNotFound
}

// Prefer JPEG/PNG forms of the first photo over a WebP thumbnail when possible.
func coverPhoto(raw json.RawMessage) string {
	var gallery struct {
		Preview []string `json:"preview"`
		Small   []string `json:"small"`
		Full    []string `json:"full"`
	}
	if json.Unmarshal(raw, &gallery) != nil {
		return ""
	}
	var fallback string
	for _, photos := range [][]string{gallery.Small, gallery.Preview, gallery.Full} {
		if len(photos) == 0 {
			continue
		}
		u, err := url.Parse(photos[0])
		if err != nil || u.Scheme != "https" || u.User != nil || u.Host != "cdn1.bazaraki.com" {
			continue
		}
		p := strings.ToLower(u.Path)
		if strings.HasSuffix(p, ".jpg") || strings.HasSuffix(p, ".jpeg") || strings.HasSuffix(p, ".png") {
			return u.String()
		}
		if fallback == "" && strings.HasSuffix(p, ".webp") {
			fallback = u.String()
		}
	}
	return fallback
}

func siteURL(raw string) *url.URL {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		raw = "https://www.bazaraki.com" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	host := strings.ToLower(u.Host)
	if host != "www.bazaraki.com" && host != "bazaraki.com" {
		return nil
	}
	u.Scheme, u.Host = "https", "www.bazaraki.com"
	u.RawQuery, u.Fragment, u.ForceQuery = "", "", false
	return u
}

func listingID(u *url.URL) int64 {
	if u == nil {
		return 0
	}
	match := advertPath.FindStringSubmatch(u.Path)
	if match == nil {
		return 0
	}
	if id, err := strconv.ParseInt(match[1], 10, 64); err == nil {
		return id
	}
	return 0
}

func eurCents(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	whole, minor, hasMinor := strings.Cut(raw, ".")
	if whole == "" || (hasMinor && (len(minor) < 1 || len(minor) > 2)) {
		return nil
	}
	for _, r := range whole + minor {
		if r < '0' || r > '9' {
			return nil
		}
	}
	cents, err := strconv.ParseInt(whole+minor+strings.Repeat("0", 2-len(minor)), 10, 64)
	if err != nil {
		return nil
	}
	return &cents
}
