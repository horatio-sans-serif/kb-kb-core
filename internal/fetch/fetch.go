// Package fetch retrieves web content and extracts it to Markdown (design §8.3).
// The production fetcher routes every connection through the SSRF guard's dialer
// Control hook, so a user-supplied URL can never reach an internal address; the
// client is injectable so tests can use an httptest (loopback) server. HTML
// extraction is intentionally basic for now — real Readability/Playwright (JS
// rendering) is a later increment.
package fetch

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"kb/internal/kb"
	"kb/internal/ssrf"
)

const (
	maxBodyBytes = 8 << 20 // 8 MiB
	maxRedirects = 5
	userAgent    = "kb-core/0 (+local fetcher)"
)

// WebFetcher fetches http(s) URLs and extracts their readable text.
type WebFetcher struct{ client *http.Client }

// NewWebFetcher builds a fetcher over the given client (injected so tests can
// reach a loopback httptest server).
func NewWebFetcher(client *http.Client) *WebFetcher { return &WebFetcher{client: client} }

// New builds the production fetcher: an http.Client whose dialer rejects internal
// destination IPs (SSRF, defeating DNS rebinding at dial time) and which caps
// redirects and timeouts.
func New() *WebFetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: ssrf.Control}
	return NewWebFetcher(&http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext},
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("fetch: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("fetch: redirect to non-http scheme %q", req.URL.Scheme)
			}
			return nil
		},
	})
}

// CanHandle reports whether ref is an http(s) URL.
func (f *WebFetcher) CanHandle(ref kb.ResourceRef) bool {
	u, err := url.Parse(ref.URL)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// Extract fetches the URL and returns its title and Markdown body.
func (f *WebFetcher) Extract(ctx context.Context, ref kb.ResourceRef) (kb.Extracted, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return kb.Extracted{}, fmt.Errorf("fetch: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return kb.Extracted{}, fmt.Errorf("fetch %s: %w", ref.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return kb.Extracted{}, fmt.Errorf("fetch %s: status %d", ref.URL, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return kb.Extracted{}, fmt.Errorf("fetch %s: read body: %w", ref.URL, err)
	}
	title, body := htmlToMarkdown(string(raw))
	return kb.Extracted{Title: title, BodyMarkdown: body, ContentType: resp.Header.Get("Content-Type")}, nil
}

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(?:script|style)>`)
	reTitle       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reBlockClose  = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|article|section)>|<br\s*/?>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]+>`)
	reInlineWS    = regexp.MustCompile(`[ \t\r\f\v]+`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

// htmlToMarkdown is a basic HTML-to-text extraction: title from <title>, body
// text with script/style removed and block tags turned into line breaks.
func htmlToMarkdown(s string) (title, body string) {
	if m := reTitle.FindStringSubmatch(s); m != nil {
		title = strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(m[1], "")))
	}
	s = reScriptStyle.ReplaceAllString(s, " ")
	s = reBlockClose.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)

	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(reInlineWS.ReplaceAllString(ln, " "))
	}
	return title, strings.TrimSpace(reBlankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
