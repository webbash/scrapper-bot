package bazaraki

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	fetchTimeout = 15 * time.Second
	maxHTMLBytes = 8 << 20
	maxRedirects = 3
)

// Fetcher downloads one HTML snapshot. Its zero value uses the standard HTTP
// transport. A caller can provide a transport, for example for offline tests.
type Fetcher struct {
	Transport http.RoundTripper
}

type HTTPStatusError struct{ StatusCode int }

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("Bazaraki returned HTTP %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

// FetchHTML fetches a rental search or listing page. It follows only redirects
// to permitted Bazaraki URLs. It does not execute scripts, store sessions, or
// retry blocked or rate-limited responses.
func (f Fetcher) FetchHTML(ctx context.Context, rawURL string) ([]byte, error) {
	target, err := parseFetchURL(rawURL)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: f.Transport,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("too many Bazaraki redirects (limit: %d)", maxRedirects)
			}
			return validateFetchURL(req.URL)
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Bazaraki request: %w", err)
	}
	req.Header.Set("User-Agent", "scrapper-bot/manual-html-fetch")
	req.Header.Set("Accept", "text/html, application/xhtml+xml")
	req.Header.Set("Accept-Language", "en")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Bazaraki HTML: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{StatusCode: resp.StatusCode}
	}
	contentType := resp.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return nil, fmt.Errorf("expected Bazaraki HTML, got Content-Type %q", contentType)
	}
	if resp.ContentLength > maxHTMLBytes {
		return nil, fmt.Errorf("Bazaraki HTML exceeds %d bytes", maxHTMLBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Bazaraki HTML: %w", err)
	}
	if len(body) > maxHTMLBytes {
		return nil, fmt.Errorf("Bazaraki HTML exceeds %d bytes", maxHTMLBytes)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("Bazaraki returned an empty HTML response")
	}
	return body, nil
}

func parseFetchURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Bazaraki URL: %w", err)
	}
	if err := validateFetchURL(u); err != nil {
		return nil, err
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

func validateFetchURL(u *url.URL) error {
	host := strings.ToLower(u.Host)
	if u.Scheme != "https" || u.User != nil || (host != "www.bazaraki.com" && host != "bazaraki.com") {
		return fmt.Errorf("expected an HTTPS URL on bazaraki.com or www.bazaraki.com without credentials or a custom port")
	}
	p := strings.TrimSuffix(u.Path, "/")
	const apartments = "/real-estate-to-rent/apartments-flats"
	if path.Clean(u.Path) != p || !(listingID(u) != 0 || p == apartments || strings.HasPrefix(p, apartments+"/")) {
		return fmt.Errorf("expected a Bazaraki apartment rental search or an /adv/ listing URL")
	}
	return nil
}
