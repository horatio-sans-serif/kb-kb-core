// Package github is a PULL source connector for a configured GitHub user's
// starred repositories via the public REST API (GET
// https://api.github.com/users/{user}/starred). The source config JSON gives the
// username ({"user": "..."}); each poll fetches the user's most-recently-starred
// repos (one page, capped at maxRepos) and emits one item per repo: the title is
// the repo full_name, the body is a small Markdown card (description, URL, primary
// language, star count), and the canonical URL is the repo html_url. Ingest dedup
// absorbs repeats across polls.
//
// Public stars need no auth. Unauthenticated GitHub is rate-limited to 60 req/hr,
// which is acceptable for a personal poll; GitHub requires a descriptive
// User-Agent on every request. A malformed repo (e.g. missing html_url) is skipped
// rather than failing the whole fetch (graceful degradation, §8.3).
package github

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
	apiBase      = "https://api.github.com"
	maxBodyBytes = 8 << 20 // 8 MiB JSON cap
	maxRepos     = 100     // per-fetch cap (one page); GitHub's default page size
	userAgent    = "kb-core/0 (+local reader)"
)

// Connector fetches a GitHub user's starred repositories. The HTTP client and base
// URL are injectable (production passes an SSRF-guarded client and the real API
// base; tests pass a loopback server).
type Connector struct {
	client *http.Client
	base   string
}

// New builds a connector over the given client, hitting the real GitHub API.
func New(client *http.Client) *Connector { return &Connector{client: client, base: apiBase} }

func (*Connector) Name() string        { return "github" }
func (*Connector) Kind() kb.SourceKind { return kb.Pull }

type config struct {
	User string `json:"user"`
}

type repo struct {
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	HTMLURL     string `json:"html_url"`
	Language    string `json:"language"`
	Stars       int    `json:"stargazers_count"`
}

// Fetch returns one item per starred repo, capped at maxRepos. The cursor is the
// number of repos emitted on the last poll (advisory only); items are re-emitted
// each poll and absorbed by ingest dedup, since the starred list has no stable
// incremental marker over the public endpoint.
func (c *Connector) Fetch(ctx context.Context, src *kb.Source, since kb.Cursor) ([]kb.RawItem, kb.Cursor, error) {
	cfg, err := parseConfig(src.Config)
	if err != nil {
		return nil, since, err
	}
	repos, err := c.starred(ctx, cfg.User)
	if err != nil {
		return nil, since, err
	}

	out := make([]kb.RawItem, 0, len(repos))
	for _, r := range repos {
		// Skip a malformed repo (no identity/URL) rather than failing the fetch.
		name := strings.TrimSpace(r.FullName)
		urlStr := strings.TrimSpace(r.HTMLURL)
		if name == "" || urlStr == "" {
			continue
		}
		out = append(out, kb.RawItem{
			Title: name,
			URL:   urlStr,
			Body:  repoBody(r),
		})
		if len(out) >= maxRepos {
			break
		}
	}

	return out, kb.Cursor(strconv.Itoa(len(out))), nil
}

// repoBody renders the repo as a small Markdown card.
func repoBody(r repo) string {
	var b strings.Builder
	if d := strings.TrimSpace(r.Description); d != "" {
		b.WriteString(d)
		b.WriteString("\n\n")
	}
	b.WriteString(strings.TrimSpace(r.HTMLURL))
	b.WriteString("\n")
	if lang := strings.TrimSpace(r.Language); lang != "" {
		b.WriteString("\nLanguage: ")
		b.WriteString(lang)
	}
	b.WriteString("\nStars: ")
	b.WriteString(strconv.Itoa(r.Stars))
	return b.String()
}

// HealthCheck reports whether the GitHub API is reachable and the response parses.
func (c *Connector) HealthCheck(ctx context.Context, src *kb.Source) (kb.Health, error) {
	cfg, err := parseConfig(src.Config)
	if err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	if _, err := c.starred(ctx, cfg.User); err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	return kb.Health{OK: true}, nil
}

func (c *Connector) starred(ctx context.Context, user string) ([]repo, error) {
	// PathEscape the owner-supplied username so a stray character can't reshape the
	// request path (the base host is fixed, but defense in depth).
	reqURL := c.base + "/users/" + url.PathEscape(user) + "/starred?per_page=" + strconv.Itoa(maxRepos)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	// GitHub requires a User-Agent and recommends pinning the API version.
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("github: read: %w", err)
	}
	var repos []repo
	if err := json.Unmarshal(body, &repos); err != nil {
		return nil, fmt.Errorf("github: parse: %w", err)
	}
	return repos, nil
}

func parseConfig(raw string) (config, error) {
	var cfg config
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return config{}, fmt.Errorf("github: parse config: %w", err)
		}
	}
	cfg.User = strings.TrimSpace(cfg.User)
	if cfg.User == "" {
		return config{}, fmt.Errorf("github: config missing \"user\"")
	}
	return cfg, nil
}
