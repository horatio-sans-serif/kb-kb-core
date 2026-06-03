// Package rss is a PULL source connector for RSS 2.0 and Atom feeds. The source's
// config JSON gives the feed URL ({"url": "..."}); each poll fetches the feed,
// returns items published after the cursor, and advances the cursor to the newest
// item's timestamp. Items carry the article link, so the enrichment pipeline's
// fetch stage retrieves the full content (the feed snippet is only a fallback for
// link-less items).
package rss

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kb/internal/kb"
)

const maxFeedBytes = 8 << 20 // 8 MiB

// Connector fetches RSS/Atom feeds. The HTTP client is injectable (production
// passes an SSRF-guarded one; tests pass a loopback client).
type Connector struct{ client *http.Client }

// New builds a connector over the given client.
func New(client *http.Client) *Connector { return &Connector{client: client} }

func (*Connector) Name() string        { return "rss" }
func (*Connector) Kind() kb.SourceKind { return kb.Pull }

// Fetch retrieves the feed and returns items newer than since, with an updated
// cursor (the newest item timestamp in RFC3339). On the first poll (empty cursor)
// all items are returned. Date-less items are returned only on the first poll, so
// later polls don't re-encounter them.
func (c *Connector) Fetch(ctx context.Context, src *kb.Source, since kb.Cursor) ([]kb.RawItem, kb.Cursor, error) {
	feedURL, err := configURL(src.Config)
	if err != nil {
		return nil, since, err
	}
	body, err := c.get(ctx, feedURL)
	if err != nil {
		return nil, since, err
	}
	items, err := parseFeed(body)
	if err != nil {
		return nil, since, err
	}

	sinceTime, hasSince := time.Time{}, false
	if since != "" {
		if t, perr := time.Parse(time.RFC3339, string(since)); perr == nil {
			sinceTime, hasSince = t, true
		}
	}

	out := make([]kb.RawItem, 0, len(items))
	newest := sinceTime
	for _, it := range items {
		switch {
		case it.published.IsZero():
			// Date-less: cannot track incrementally, so always emit and let ingest
			// dedup absorb repeats — losing a new date-less item is worse.
		case hasSince && it.published.Before(sinceTime):
			continue // strictly older than the cursor — already seen
		default:
			// Inclusive boundary: an item AT the cursor second is re-emitted (and
			// deduped) rather than dropped, so a second item published in the same
			// second as the last cursor is never lost.
			if it.published.After(newest) {
				newest = it.published
			}
		}
		out = append(out, kb.RawItem{URL: resolveLink(feedURL, it.link), Title: it.title, Body: it.body})
	}

	cursor := since
	if !newest.IsZero() {
		cursor = kb.Cursor(newest.UTC().Format(time.RFC3339))
	}
	return out, cursor, nil
}

// resolveLink makes a relative item link absolute against the feed URL; an
// already-absolute or unparseable link is returned unchanged.
func resolveLink(feedURL, link string) string {
	if link == "" {
		return ""
	}
	base, err := url.Parse(feedURL)
	if err != nil {
		return link
	}
	ref, err := url.Parse(link)
	if err != nil {
		return link
	}
	return base.ResolveReference(ref).String()
}

// HealthCheck reports whether the feed is reachable and parseable.
func (c *Connector) HealthCheck(ctx context.Context, src *kb.Source) (kb.Health, error) {
	feedURL, err := configURL(src.Config)
	if err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	body, err := c.get(ctx, feedURL)
	if err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	if _, err := parseFeed(body); err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	return kb.Health{OK: true}, nil
}

func (c *Connector) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("rss: build request: %w", err)
	}
	req.Header.Set("User-Agent", "kb-core/0 (+local feed reader)")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rss: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rss: %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
}

func configURL(config string) (string, error) {
	var cfg struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("rss: parse config: %w", err)
	}
	cfg.URL = strings.TrimSpace(cfg.URL)
	if cfg.URL == "" {
		return "", fmt.Errorf("rss: config missing \"url\"")
	}
	// Reject non-http(s) feed URLs early (e.g. file://, gopher://). The production
	// client's dialer also blocks internal IPs; this rejects bad schemes on the
	// first hop, which the dialer's redirect-only scheme check does not cover.
	if u, err := url.Parse(cfg.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("rss: feed url must be http or https")
	}
	return cfg.URL, nil
}

type feedItem struct {
	title, link, body string
	published         time.Time
}

func parseFeed(b []byte) ([]feedItem, error) {
	var r rssFeed
	if xml.Unmarshal(b, &r) == nil && len(r.Channel.Items) > 0 {
		return r.items(), nil
	}
	var a atomFeed
	if xml.Unmarshal(b, &a) == nil && len(a.Entries) > 0 {
		return a.items(), nil
	}
	return nil, fmt.Errorf("rss: no items found (not RSS 2.0 or Atom?)")
}

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			PubDate     string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
}

func (r rssFeed) items() []feedItem {
	out := make([]feedItem, 0, len(r.Channel.Items))
	for _, it := range r.Channel.Items {
		out = append(out, feedItem{
			title:     strings.TrimSpace(it.Title),
			link:      strings.TrimSpace(it.Link),
			body:      it.Description,
			published: parseTime(it.PubDate),
		})
	}
	return out
}

type atomFeed struct {
	XMLName xml.Name `xml:"feed"`
	Entries []struct {
		Title   string `xml:"title"`
		Updated string `xml:"updated"`
		Summary string `xml:"summary"`
		Content struct {
			// Captures both type="html" (text) and type="xhtml" (nested markup) —
			// innerxml grabs the raw inner content of <content> either way.
			Inner string `xml:",innerxml"`
		} `xml:"content"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func (a atomFeed) items() []feedItem {
	out := make([]feedItem, 0, len(a.Entries))
	for _, e := range a.Entries {
		body := strings.TrimSpace(e.Content.Inner)
		if body == "" {
			body = e.Summary
		}
		out = append(out, feedItem{
			title:     strings.TrimSpace(e.Title),
			link:      atomLink(e.Links),
			body:      body,
			published: parseTime(e.Updated),
		})
	}
	return out
}

// atomLink picks the alternate (or first) link href.
func atomLink(links []struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}) string {
	for _, l := range links {
		if l.Rel == "alternate" || l.Rel == "" {
			return strings.TrimSpace(l.Href)
		}
	}
	if len(links) > 0 {
		return strings.TrimSpace(links[0].Href)
	}
	return ""
}

// parseTime parses the common feed date formats; an unparseable value yields the
// zero time (the connector treats those items as date-less).
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC822Z, time.RFC822} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
