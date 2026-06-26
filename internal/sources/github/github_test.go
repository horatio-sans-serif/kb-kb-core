package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kb/internal/kb"
)

// starredJSON is a small fixture of GitHub's /users/{user}/starred response. The
// third entry is malformed (no html_url) and must be skipped, not fatal.
const starredJSON = `[
  {"full_name":"torvalds/linux","description":"Linux kernel source tree","html_url":"https://github.com/torvalds/linux","language":"C","stargazers_count":170000},
  {"full_name":"golang/go","description":"The Go programming language","html_url":"https://github.com/golang/go","language":"Go","stargazers_count":120000},
  {"full_name":"broken/repo","description":"missing url","html_url":"","language":"Rust","stargazers_count":5}
]`

func testConnector(t *testing.T, handler http.HandlerFunc) *Connector {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &Connector{client: ts.Client(), base: ts.URL}
}

func TestGitHub_Contract(t *testing.T) {
	c := &Connector{}
	if c.Name() != "github" || c.Kind() != kb.Pull {
		t.Errorf("contract = %q/%q", c.Name(), c.Kind())
	}
}

func TestGitHub_FetchEmitsArtifacts(t *testing.T) {
	var gotPath, gotUA string
	c := testConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(starredJSON))
	})
	src := &kb.Source{Config: `{"user":"octocat"}`}

	items, cursor, err := c.Fetch(context.Background(), src, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/users/octocat/starred" {
		t.Errorf("path = %q, want /users/octocat/starred", gotPath)
	}
	if gotUA == "" {
		t.Error("GitHub requires a User-Agent header; none sent")
	}
	// The malformed third repo is skipped.
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (malformed repo skipped)", len(items))
	}
	if items[0].Title != "torvalds/linux" || items[0].URL != "https://github.com/torvalds/linux" {
		t.Errorf("item0 = %+v", items[0])
	}
	// Body markdown carries description, url, language and star count.
	body := items[0].Body
	for _, want := range []string{"Linux kernel source tree", "https://github.com/torvalds/linux", "C", "170000"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
	// Cursor is non-empty after a successful first poll.
	if cursor == "" {
		t.Error("cursor should be set after a fetch")
	}
}

func TestGitHub_MissingUserErrors(t *testing.T) {
	c := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	if _, _, err := c.Fetch(context.Background(), &kb.Source{Config: `{}`}, ""); err == nil {
		t.Error("fetch with no configured user should error")
	}
}

func TestGitHub_FetchCapsItems(t *testing.T) {
	// Build a response larger than the per-fetch cap and confirm it is bounded.
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < maxRepos+25; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"full_name":"u/r","description":"d","html_url":"https://github.com/u/r","language":"Go","stargazers_count":1}`)
	}
	b.WriteString("]")
	c := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	})
	items, _, err := c.Fetch(context.Background(), &kb.Source{Config: `{"user":"octocat"}`}, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) > maxRepos {
		t.Errorf("items = %d, want <= cap %d", len(items), maxRepos)
	}
}

func TestGitHub_HealthCheck(t *testing.T) {
	c := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	h, err := c.HealthCheck(context.Background(), &kb.Source{Config: `{"user":"octocat"}`})
	if err != nil || !h.OK {
		t.Errorf("healthcheck = %+v, %v, want OK", h, err)
	}

	bad := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if h, _ := bad.HealthCheck(context.Background(), &kb.Source{Config: `{"user":"octocat"}`}); h.OK || !strings.Contains(h.Detail, "status") {
		t.Errorf("unhealthy check = %+v, want not-OK with a status detail", h)
	}
}
