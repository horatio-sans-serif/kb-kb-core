// Package hn is a PULL source connector for Hacker News via the public Algolia
// search API (no auth). Each poll fetches the most recent stories for the
// configured tag, filters to those newer than the cursor and above an optional
// minimum points, and advances the cursor to the newest item's creation time.
// Linked stories carry their article URL (the fetch stage retrieves the content);
// text posts (Ask/Show HN) carry their body. The endpoint is fixed, so the source
// config only tunes the query — no arbitrary URL, unlike the RSS connector.
package hn

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
	algoliaBase  = "https://hn.algolia.com/api/v1/search_by_date"
	maxBodyBytes = 4 << 20 // 4 MiB JSON cap
	hitsPerPage  = 50
	defaultTags  = "front_page" // curated front-page stories; "story" for all new
)

// Connector fetches Hacker News stories. The HTTP client and base URL are
// injectable (production passes an SSRF-guarded client and the real base; tests
// pass a loopback server).
type Connector struct {
	client *http.Client
	base   string
}

// New builds a connector over the given client, hitting the real Algolia endpoint.
func New(client *http.Client) *Connector { return &Connector{client: client, base: algoliaBase} }

func (*Connector) Name() string        { return "hackernews" }
func (*Connector) Kind() kb.SourceKind { return kb.Pull }

type config struct {
	Tags      string `json:"tags"`
	MinPoints int    `json:"min_points"`
}

type hit struct {
	ObjectID  string `json:"objectID"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Points    int    `json:"points"`
	CreatedAt int64  `json:"created_at_i"`
	StoryText string `json:"story_text"`
}

// Fetch returns stories newer than since (inclusive at the boundary second, so a
// same-second item is re-emitted and deduped rather than lost) and above the
// configured minimum points, with the cursor advanced to the newest creation time
// (a unix-second string). The first poll (empty cursor) emits all returned hits.
func (c *Connector) Fetch(ctx context.Context, src *kb.Source, since kb.Cursor) ([]kb.RawItem, kb.Cursor, error) {
	cfg, err := parseConfig(src.Config)
	if err != nil {
		return nil, since, err
	}
	hits, err := c.search(ctx, cfg.Tags)
	if err != nil {
		return nil, since, err
	}

	var sinceTS int64
	if since != "" {
		sinceTS, _ = strconv.ParseInt(string(since), 10, 64)
	}

	out := make([]kb.RawItem, 0, len(hits))
	newest := sinceTS
	for _, h := range hits {
		if h.Points < cfg.MinPoints {
			continue
		}
		if since != "" && h.CreatedAt < sinceTS {
			continue // strictly older than the cursor — already seen
		}
		title := strings.TrimSpace(h.Title)
		if title == "" {
			continue
		}
		item := kb.RawItem{Title: title}
		switch {
		case strings.TrimSpace(h.URL) != "":
			item.URL = h.URL // a linked story: fetch the article
		case strings.TrimSpace(h.StoryText) != "":
			item.Body = h.StoryText // a text post: its body is the content
		default:
			item.URL = "https://news.ycombinator.com/item?id=" + url.QueryEscape(h.ObjectID) // discussion fallback
		}
		out = append(out, item)
		if h.CreatedAt > newest {
			newest = h.CreatedAt
		}
	}

	cursor := since
	if newest > sinceTS || (since == "" && newest > 0) {
		cursor = kb.Cursor(strconv.FormatInt(newest, 10))
	}
	return out, cursor, nil
}

// HealthCheck reports whether the Algolia endpoint is reachable and parseable.
func (c *Connector) HealthCheck(ctx context.Context, src *kb.Source) (kb.Health, error) {
	cfg, err := parseConfig(src.Config)
	if err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	if _, err := c.search(ctx, cfg.Tags); err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	return kb.Health{OK: true}, nil
}

func (c *Connector) search(ctx context.Context, tags string) ([]hit, error) {
	q := url.Values{
		"tags":        {tags},
		"hitsPerPage": {strconv.Itoa(hitsPerPage)},
	}
	reqURL := c.base + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("hn: build request: %w", err)
	}
	req.Header.Set("User-Agent", "kb-core/0 (+local reader)")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hn: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hn: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("hn: read: %w", err)
	}
	var page struct {
		Hits []hit `json:"hits"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("hn: parse: %w", err)
	}
	return page.Hits, nil
}

func parseConfig(raw string) (config, error) {
	cfg := config{Tags: defaultTags}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return config{}, fmt.Errorf("hn: parse config: %w", err)
		}
	}
	if strings.TrimSpace(cfg.Tags) == "" {
		cfg.Tags = defaultTags
	}
	return cfg, nil
}
