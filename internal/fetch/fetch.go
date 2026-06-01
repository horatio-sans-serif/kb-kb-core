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
	"mime"
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

// WebFetcher fetches http(s) URLs and extracts their readable text. A non-nil
// guard pre-validates the target hostname against the SSRF block list before the
// request is built; the production fetcher also wires ssrf.Control into the
// dialer so the actually-dialed IP is re-checked (rebinding). The injectable
// (test) constructor leaves guard nil so a loopback httptest server is reachable.
type WebFetcher struct {
	client *http.Client
	guard  *ssrf.Guard
}

// NewWebFetcher builds a fetcher over the given client (injected so tests can
// reach a loopback httptest server). It has no SSRF guard — the caller controls
// the destination — so production code must use New instead.
func NewWebFetcher(client *http.Client) *WebFetcher { return &WebFetcher{client: client} }

// New builds the production fetcher: an http.Client whose dialer rejects internal
// destination IPs (SSRF, defeating DNS rebinding at dial time) and which caps
// redirects and timeouts. It also carries an ssrf.Guard so the hostname is
// rejected before any connection is attempted.
func New() *WebFetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: ssrf.Control}
	f := NewWebFetcher(&http.Client{
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
	f.guard = ssrf.New()
	return f
}

// CanHandle reports whether ref is an http(s) URL.
func (f *WebFetcher) CanHandle(ref kb.ResourceRef) bool {
	u, err := url.Parse(ref.URL)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// Extract fetches the URL and returns its title and Markdown body. It rejects a
// blocked host (when a guard is configured), a non-2xx status, a non-HTML
// content type, and a body that exceeds the size cap (rather than silently
// truncating it).
func (f *WebFetcher) Extract(ctx context.Context, ref kb.ResourceRef) (kb.Extracted, error) {
	if f.guard != nil {
		if err := f.guard.CheckURL(ctx, ref.URL); err != nil {
			return kb.Extracted{}, fmt.Errorf("fetch %s: %w", ref.URL, err)
		}
	}

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
	ctype := resp.Header.Get("Content-Type")
	if err := checkHTMLContentType(ctype); err != nil {
		return kb.Extracted{}, fmt.Errorf("fetch %s: %w", ref.URL, err)
	}

	// Read one byte past the cap to distinguish a body at the limit from one that
	// overflows it, and fail loud rather than truncating to a misleading partial.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return kb.Extracted{}, fmt.Errorf("fetch %s: read body: %w", ref.URL, err)
	}
	if len(raw) > maxBodyBytes {
		return kb.Extracted{}, fmt.Errorf("fetch %s: body exceeds %d bytes", ref.URL, maxBodyBytes)
	}
	title, body := htmlToMarkdown(string(raw))
	return kb.Extracted{Title: title, BodyMarkdown: body, ContentType: ctype}, nil
}

// checkHTMLContentType accepts an empty/missing type (some servers omit it) and
// any text/* or (X)HTML/XML media type; it rejects everything else (images,
// PDFs, octet-streams) so the HTML extractor never runs on binary content. Those
// types are handled by their own fetchers in later increments.
func checkHTMLContentType(ct string) error {
	if ct == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return fmt.Errorf("unparseable content type %q", ct)
	}
	switch {
	case strings.HasPrefix(mediaType, "text/"),
		mediaType == "application/xhtml+xml",
		mediaType == "application/xml":
		return nil
	}
	return fmt.Errorf("unsupported content type %q", mediaType)
}

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(?:script|style|noscript)>`)
	reHead        = regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`)
	reTitle       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reBlockClose  = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|article|section)>|<br\s*/?>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]+>`)
	reInlineWS    = regexp.MustCompile(`[ \t\r\f\v]+`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

// htmlToMarkdown is a basic HTML-to-text extraction: title from <title>, body
// text with the head section and script/style removed and block tags turned into
// line breaks. The head is dropped after the title is captured so head content
// (title, meta) does not leak into — and pollute the tokens of — the body.
func htmlToMarkdown(s string) (title, body string) {
	if m := reTitle.FindStringSubmatch(s); m != nil {
		title = strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(m[1], "")))
	}
	s = reHead.ReplaceAllString(s, " ")
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
