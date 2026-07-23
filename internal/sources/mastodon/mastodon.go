// Package mastodon is a PULL source connector for a configured account's recent
// public posts (statuses) on a Mastodon instance, via the public REST API (no
// auth). The source config JSON gives the instance host and account handle
// ({"instance":"mastodon.social","acct":"Gargron"}); each poll runs the two-step
// public flow — resolve the handle to an account id
// (GET https://{instance}/api/v1/accounts/lookup?acct={acct}), then read that
// account's recent statuses
// (GET https://{instance}/api/v1/accounts/{id}/statuses?limit=N&exclude_replies=true&exclude_reblogs=true)
// — and emits one item per status (capped at maxStatuses): the title is a short
// title derived from the first line of the HTML-stripped content, the body is the
// status content stripped to plain text, and the canonical URL is the status url.
// Ingest dedup absorbs repeats across polls.
//
// Public accounts need no auth. Mastodon status content is HTML; it is stripped to
// text with a minimal standard-library stripper (no new dependency). A malformed
// status (e.g. missing url) is skipped rather than failing the whole fetch
// (graceful degradation, §8.3). The instance host is owner-supplied config; the
// base is built as https://{instance} and user-supplied path/query segments are
// escaped (defense in depth; the SSRF-guarded client is the primary control).
package mastodon

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"kb/internal/kb"
)

const (
	maxBodyBytes = 8 << 20 // 8 MiB JSON cap
	maxStatuses  = 40      // per-fetch cap (one page)
	maxTitleLen  = 120     // derived-title truncation length
	userAgent    = "kb-core/0 (+local reader)"
)

// Connector fetches a Mastodon account's recent public statuses. The HTTP client,
// URL scheme, and instance host are injectable (production passes an SSRF-guarded
// client, "https", and the configured instance; tests pass a loopback server).
type Connector struct {
	client *http.Client
	scheme string // "https" in production; "http" for the loopback test server
	base   string // instance host, overrides the config when set (tests only)
}

// New builds a connector over the given client. The instance host comes from each
// source's config; requests use https.
func New(client *http.Client) *Connector { return &Connector{client: client, scheme: "https"} }

func (*Connector) Name() string        { return "mastodon" }
func (*Connector) Kind() kb.SourceKind { return kb.Pull }

type config struct {
	Instance string `json:"instance"`
	Acct     string `json:"acct"`
}

type account struct {
	ID string `json:"id"`
}

type status struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// Fetch runs the two-step public flow and returns one item per status, capped at
// maxStatuses. The cursor is the number of items emitted on the last poll
// (advisory only); items are re-emitted each poll and absorbed by ingest dedup,
// since the public statuses endpoint has no stable incremental marker used here.
func (c *Connector) Fetch(ctx context.Context, src *kb.Source, since kb.Cursor) ([]kb.RawItem, kb.Cursor, error) {
	cfg, err := parseConfig(src.Config)
	if err != nil {
		return nil, since, err
	}
	host := c.host(cfg)
	id, err := c.lookup(ctx, host, cfg.Acct)
	if err != nil {
		return nil, since, err
	}
	statuses, err := c.statuses(ctx, host, id)
	if err != nil {
		return nil, since, err
	}

	out := make([]kb.RawItem, 0, len(statuses))
	for _, s := range statuses {
		// Skip a malformed status (no canonical URL) rather than failing the fetch.
		urlStr := strings.TrimSpace(s.URL)
		if urlStr == "" {
			continue
		}
		body := stripHTML(s.Content)
		out = append(out, kb.RawItem{
			Title: deriveTitle(body),
			URL:   urlStr,
			Body:  body,
		})
		if len(out) >= maxStatuses {
			break
		}
	}

	return out, kb.Cursor(strconv.Itoa(len(out))), nil
}

// HealthCheck reports whether the instance is reachable and the lookup parses.
func (c *Connector) HealthCheck(ctx context.Context, src *kb.Source) (kb.Health, error) {
	cfg, err := parseConfig(src.Config)
	if err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	if _, err := c.lookup(ctx, c.host(cfg), cfg.Acct); err != nil {
		return kb.Health{OK: false, Detail: err.Error()}, nil
	}
	return kb.Health{OK: true}, nil
}

// host returns the instance host to hit: the injected base (tests) when set,
// otherwise the source-configured instance.
func (c *Connector) host(cfg config) string {
	if c.base != "" {
		return c.base
	}
	return cfg.Instance
}

// lookup resolves an account handle to its numeric id.
func (c *Connector) lookup(ctx context.Context, host, acct string) (string, error) {
	reqURL := c.scheme + "://" + host + "/api/v1/accounts/lookup?acct=" + url.QueryEscape(acct)
	var acc account
	if err := c.getJSON(ctx, reqURL, &acc); err != nil {
		return "", err
	}
	id := strings.TrimSpace(acc.ID)
	if id == "" {
		return "", fmt.Errorf("mastodon: lookup returned no account id")
	}
	return id, nil
}

// statuses reads the account's recent public statuses (replies and reblogs
// excluded).
func (c *Connector) statuses(ctx context.Context, host, id string) ([]status, error) {
	// PathEscape the resolved id so a stray character can't reshape the request
	// path (the host is fixed per source, but defense in depth).
	reqURL := c.scheme + "://" + host + "/api/v1/accounts/" + url.PathEscape(id) +
		"/statuses?limit=" + strconv.Itoa(maxStatuses) + "&exclude_replies=true&exclude_reblogs=true"
	var out []status
	if err := c.getJSON(ctx, reqURL, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// getJSON issues a GET with the descriptive User-Agent and decodes the (size-
// capped) JSON body into v.
func (c *Connector) getJSON(ctx context.Context, reqURL string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("mastodon: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("mastodon: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mastodon: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("mastodon: read: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("mastodon: parse: %w", err)
	}
	return nil
}

// deriveTitle builds a short title from the first non-empty line of the stripped
// body, truncated to maxTitleLen.
func deriveTitle(body string) string {
	line := body
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > maxTitleLen {
		line = strings.TrimSpace(string(r[:maxTitleLen]))
	}
	return line
}

// stripHTML reduces Mastodon status HTML to plain text. It is a single pass, NOT a
// general sanitizer: tags are dropped (br/p/div become newlines), the CONTENT of
// script/style elements is dropped entirely, and entity references are decoded
// per-text-run — decoding only the runs OUTSIDE tags, never with a final global
// UnescapeString, so an entity-encoded "&lt;script&gt;" in the visible text becomes
// the literal characters "<script>" and is never re-interpreted as a live tag.
func stripHTML(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '<' {
			j := strings.IndexByte(s[i:], '>')
			if j < 0 {
				break // unclosed tag: drop the remainder
			}
			name := tagNameOf(s[i : i+j+1])
			if name == "script" || name == "style" {
				// Drop the element's content up to its close tag (or the end).
				rest := s[i+j+1:]
				if k := indexFold(rest, "</"+name); k >= 0 {
					if end := strings.IndexByte(rest[k:], '>'); end >= 0 {
						i = i + j + 1 + k + end + 1
						continue
					}
				}
				i = len(s)
				continue
			}
			if isBreakTag(s[i : i+j+1]) {
				b.WriteByte('\n')
			}
			i += j + 1
			continue
		}
		// Text run up to the next tag; decode entities within this run only.
		run := s[i:]
		if next := strings.IndexByte(s[i:], '<'); next >= 0 {
			run = s[i : i+next]
			i += next
		} else {
			i = len(s)
		}
		b.WriteString(html.UnescapeString(run))
	}
	text := b.String()
	// Collapse runs of 3+ newlines to a paragraph break and trim edges.
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}

// tagNameOf returns the lowercased element name of a "<...>" tag, without the
// leading slash or any attributes.
func tagNameOf(tag string) string {
	t := strings.ToLower(strings.Trim(tag, "<>/ \t\r\n"))
	if i := strings.IndexAny(t, " \t\r\n/"); i >= 0 {
		t = t[:i]
	}
	return t
}

// indexFold is strings.Index with an ASCII-case-insensitive needle.
func indexFold(haystack, needle string) int {
	return strings.Index(strings.ToLower(haystack), strings.ToLower(needle))
}

// isBreakTag reports whether a tag (including the angle brackets) introduces a
// line/paragraph break in stripped output: <br>, <p>/</p>, and <div>/</div>.
// Both the opening and closing block tags emit a newline so adjacent paragraphs
// separate; runs are collapsed afterward.
func isBreakTag(tag string) bool {
	t := strings.ToLower(strings.Trim(tag, "<>/ "))
	// Drop any attributes: keep the tag name only.
	if i := strings.IndexAny(t, " \t\r\n"); i >= 0 {
		t = t[:i]
	}
	switch t {
	case "br", "p", "div":
		return true
	}
	return false
}

func parseConfig(raw string) (config, error) {
	var cfg config
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return config{}, fmt.Errorf("mastodon: parse config: %w", err)
		}
	}
	cfg.Instance = strings.TrimSpace(cfg.Instance)
	cfg.Acct = strings.TrimSpace(cfg.Acct)
	if cfg.Instance == "" {
		return config{}, fmt.Errorf("mastodon: config missing \"instance\"")
	}
	// Instance is concatenated into the base URL as the host, so it must be a bare
	// host — no scheme, path, query, credentials, or spaces — or it could reshape the
	// request (e.g. "host/x?y=" or "good@evil"). SSRF is still caught at dial time,
	// but reject a malformed instance up front rather than send a wrong request.
	if strings.ContainsAny(cfg.Instance, "/?#@ \t") || strings.Contains(cfg.Instance, "://") {
		return config{}, fmt.Errorf("mastodon: \"instance\" must be a bare host (got %q)", cfg.Instance)
	}
	if cfg.Acct == "" {
		return config{}, fmt.Errorf("mastodon: config missing \"acct\"")
	}
	return cfg, nil
}
