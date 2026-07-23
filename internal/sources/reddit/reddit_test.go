package reddit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kb/internal/kb"
)

// listingJSON is a small fixture of Reddit's /r/{subreddit}/new.json response. The
// third child is stickied and the fourth is malformed (no title/permalink); both
// must be skipped, not fatal.
const listingJSON = `{
  "kind": "Listing",
  "data": {
    "children": [
      {"kind":"t3","data":{"title":"Go 1.24 released","selftext":"Big news","url":"https://go.dev/blog/go1.24","permalink":"/r/golang/comments/aaa/go124/","stickied":false}},
      {"kind":"t3","data":{"title":"Question about goroutines","selftext":"","url":"https://www.reddit.com/r/golang/comments/bbb/","permalink":"/r/golang/comments/bbb/goroutines/","stickied":false}},
      {"kind":"t3","data":{"title":"PINNED: read the rules","selftext":"rules here","url":"https://reddit.com/r/golang","permalink":"/r/golang/comments/ccc/rules/","stickied":true}},
      {"kind":"t3","data":{"title":"","selftext":"no permalink","url":"","permalink":"","stickied":false}}
    ]
  }
}`

func testConnector(t *testing.T, handler http.HandlerFunc) *Connector {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &Connector{client: ts.Client(), base: ts.URL}
}

func TestReddit_Contract(t *testing.T) {
	c := &Connector{}
	if c.Name() != "reddit" || c.Kind() != kb.Pull {
		t.Errorf("contract = %q/%q", c.Name(), c.Kind())
	}
}

func TestReddit_FetchEmitsArtifacts(t *testing.T) {
	var gotPath, gotUA string
	c := testConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(listingJSON))
	})
	src := &kb.Source{Config: `{"subreddit":"golang"}`}

	items, cursor, err := c.Fetch(context.Background(), src, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/r/golang/new.json" {
		t.Errorf("path = %q, want /r/golang/new.json", gotPath)
	}
	if gotUA == "" {
		t.Error("Reddit requires a User-Agent header; none sent")
	}
	// The stickied third child and malformed fourth child are skipped.
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (stickied + malformed skipped)", len(items))
	}
	if items[0].Title != "Go 1.24 released" {
		t.Errorf("item0 title = %q", items[0].Title)
	}
	// Canonical URL is the full permalink.
	if items[0].URL != "https://www.reddit.com/r/golang/comments/aaa/go124/" {
		t.Errorf("item0 url = %q", items[0].URL)
	}
	// Body markdown carries selftext, external URL, and permalink.
	body := items[0].Body
	for _, want := range []string{"Big news", "https://go.dev/blog/go1.24", "https://www.reddit.com/r/golang/comments/aaa/go124/"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
	// Cursor is non-empty after a successful first poll.
	if cursor == "" {
		t.Error("cursor should be set after a fetch")
	}
}

func TestReddit_MissingSubredditErrors(t *testing.T) {
	c := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"children":[]}}`))
	})
	if _, _, err := c.Fetch(context.Background(), &kb.Source{Config: `{}`}, ""); err == nil {
		t.Error("fetch with no configured subreddit should error")
	}
}

func TestReddit_FetchCapsItems(t *testing.T) {
	// Build a listing larger than the per-fetch cap and confirm it is bounded.
	var b strings.Builder
	b.WriteString(`{"data":{"children":[`)
	for i := 0; i < maxPosts+25; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"kind":"t3","data":{"title":"t","selftext":"s","url":"https://x","permalink":"/r/golang/comments/z/","stickied":false}}`)
	}
	b.WriteString(`]}}`)
	c := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	})
	items, _, err := c.Fetch(context.Background(), &kb.Source{Config: `{"subreddit":"golang"}`}, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) > maxPosts {
		t.Errorf("items = %d, want <= cap %d", len(items), maxPosts)
	}
}

func TestReddit_HealthCheck(t *testing.T) {
	c := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"children":[]}}`))
	})
	h, err := c.HealthCheck(context.Background(), &kb.Source{Config: `{"subreddit":"golang"}`})
	if err != nil || !h.OK {
		t.Errorf("healthcheck = %+v, %v, want OK", h, err)
	}

	bad := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if h, _ := bad.HealthCheck(context.Background(), &kb.Source{Config: `{"subreddit":"golang"}`}); h.OK || !strings.Contains(h.Detail, "status") {
		t.Errorf("unhealthy check = %+v, want not-OK with a status detail", h)
	}
}
