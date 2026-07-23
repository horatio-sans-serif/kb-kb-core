// Package reddit is a PULL source connector for a configured subreddit's new
// posts via the public JSON endpoint (GET
// https://www.reddit.com/r/{subreddit}/new.json?limit=N). The source config JSON
// gives the subreddit ({"subreddit": "..."}); each poll fetches the most recent
// posts (capped at maxPosts) and emits one item per post: the title is the post
// title, the body is a small Markdown card (selftext when present, the post's
// external URL, and its permalink), and the canonical URL is the full permalink
// (https://www.reddit.com + permalink). Ingest dedup absorbs repeats across polls.
//
// The public endpoint needs no auth but Reddit requires a descriptive User-Agent
// on every request and rate-limits, so each poll reads at most maxPosts of the
// most-recent posts (no deep paging). A stickied post (pinned announcement) or a
// malformed post (missing title/permalink) is skipped rather than failing the
// whole fetch (graceful degradation, §8.3).
package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"kb/internal/kb"
)

const (
	apiBase      = "https://www.reddit.com"
	maxBodyBytes = 8 << 20 // 8 MiB JSON cap
	maxPosts     = 100     // per-fetch cap
	userAgent    = "kb-core/0 (+local reader)"
)

// Connector fetches a subreddit's new posts. The HTTP client and base URL are
// injectable (production passes an SSRF-guarded client and the real base; tests
// pass a loopback server).
type Connector struct {
	client *http.Client
	base   string
}

// New builds a connector over the given client, hitting the real Reddit endpoint.
func New(client *http.Client) *Connector { return &Connector{client: client, base: apiBase} }

func (*Connector) Name() string        { return "reddit" }
func (*Connector) Kind() kb.SourceKind { return kb.Pull }

type config struct {
	Subreddit string `json:"subreddit"`
}

type post struct {
	Title     string `json:"title"`
	Selftext  string `json:"selftext"`
	URL       string `json:"url"`
	Permalink string `json:"permalink"`
	Stickied  bool   `json:"stickied"`
}

// listing is the standard Reddit listing shape: data.children[].data.
type listing struct {
	Data struct {
		Children []struct {
			Data post `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

// Fetch returns one item per new post, capped at maxPosts. Stickied and malformed
// posts are skipped. The cursor is the number of posts emitted on the last poll
// (advisory only); items are re-emitted each poll and absorbed by ingest dedup,
// since the new-posts listing has no stable incremental marker over the public
// endpoint.
func (c *Connector) Fetch(ctx context.Context, src *kb.Source, since kb.Cursor) ([]kb.RawItem, kb.Cursor, error) {
	cfg, err := parseConfig(src.Config)
	if err != nil {
		return nil, since, err
	}
	posts, err := c.newPosts(ctx, cfg.Subreddit)
	if err != nil {
		return nil, since, err
	}

	out := make([]kb.RawItem, 0, len(posts))
	for _, p := range posts {
		if p.Stickied {
			continue // pinned announcement, not a real new post
		}
		// Skip a malformed post rather than failing the fetch. The permalink must be a
		// host-relative path ("/r/..."); reject "//x" or "@x", which would escape the
		// reddit host when concatenated onto apiBase and let a hostile response point
		// the canonical URL elsewhere.
		title := strings.TrimSpace(p.Title)
		permalink := strings.TrimSpace(p.Permalink)
		if title == "" || !strings.HasPrefix(permalink, "/") || strings.HasPrefix(permalink, "//") {
			continue
		}
		permalinkURL := apiBase + permalink
		out = append(out, kb.RawItem{
			Title: title,
			URL:   permalinkURL,
			Body:  postBody(p, permalinkURL),
		})
		if len(out) >= maxPosts {
			break
		}
	}

	return out, kb.Cursor(strconv.Itoa(len(out))), nil
}

// postBody renders the post as a small Markdown card. permalinkURL is the already-
// validated canonical URL (built once by the caller).
func postBody(p post, permalinkURL string) string {
	var b strings.Builder
	if s := strings.TrimSpace(p.Selftext); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	if u := strings.TrimSpace(p.URL); u != "" {
		b.WriteString("URL: ")
		b.WriteString(u)
		b.WriteString("\n")
	}
	b.WriteString("Permalink: ")
	b.WriteString(permalinkURL)
	return b.String()
}

// HealthCheck reports whether the Reddit endpoint is reachable and parseable.
func (c *Connector) HealthCheck(ctx context.Context, src *kb.Source) (kb.Health, error) {
	cfg, err := parseConfig(src.Config)
	if err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	if _, err := c.newPosts(ctx, cfg.Subreddit); err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	return kb.Health{OK: true}, nil
}

func (c *Connector) newPosts(ctx context.Context, subreddit string) ([]post, error) {
	// PathEscape the owner-supplied subreddit so a stray character can't reshape the
	// request path (the base host is fixed, but defense in depth).
	reqURL := c.base + "/r/" + url.PathEscape(subreddit) + "/new.json?limit=" + strconv.Itoa(maxPosts)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("reddit: build request: %w", err)
	}
	// Reddit requires a descriptive User-Agent and rate-limits anonymous requests.
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reddit: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reddit: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("reddit: read: %w", err)
	}
	var lst listing
	if err := json.Unmarshal(body, &lst); err != nil {
		return nil, fmt.Errorf("reddit: parse: %w", err)
	}
	posts := make([]post, 0, len(lst.Data.Children))
	for _, ch := range lst.Data.Children {
		posts = append(posts, ch.Data)
	}
	return posts, nil
}

func parseConfig(raw string) (config, error) {
	var cfg config
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return config{}, fmt.Errorf("reddit: parse config: %w", err)
		}
	}
	cfg.Subreddit = strings.TrimSpace(cfg.Subreddit)
	if cfg.Subreddit == "" {
		return config{}, fmt.Errorf("reddit: config missing \"subreddit\"")
	}
	return cfg, nil
}
