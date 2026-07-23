package mastodon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kb/internal/kb"
)

// lookupJSON is a small fixture of Mastodon's /api/v1/accounts/lookup response:
// the account handle resolves to a numeric id.
const lookupJSON = `{"id":"1","username":"Gargron","acct":"Gargron"}`

// statusesJSON is a small fixture of /api/v1/accounts/{id}/statuses. The content
// is HTML (as Mastodon serves it); the third entry is malformed (no url) and must
// be skipped, not fatal.
const statusesJSON = `[
  {"id":"101","url":"https://mastodon.social/@Gargron/101","content":"<p>Hello <strong>world</strong>!</p><p>Second paragraph.</p>"},
  {"id":"102","url":"https://mastodon.social/@Gargron/102","content":"<p>Just a &amp; short note</p>"},
  {"id":"103","url":"","content":"<p>missing url</p>"}
]`

func testConnector(t *testing.T, handler http.HandlerFunc) *Connector {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &Connector{client: ts.Client(), scheme: "http", base: ts.Listener.Addr().String()}
}

func TestMastodon_Contract(t *testing.T) {
	c := &Connector{}
	if c.Name() != "mastodon" || c.Kind() != kb.Pull {
		t.Errorf("contract = %q/%q", c.Name(), c.Kind())
	}
}

func TestMastodon_FetchEmitsArtifacts(t *testing.T) {
	var gotLookupPath, gotLookupQuery, gotStatusesPath, gotUA string
	c := testConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		switch {
		case r.URL.Path == "/api/v1/accounts/lookup":
			gotLookupPath = r.URL.Path
			gotLookupQuery = r.URL.Query().Get("acct")
			_, _ = w.Write([]byte(lookupJSON))
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			gotStatusesPath = r.URL.Path
			_, _ = w.Write([]byte(statusesJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	src := &kb.Source{Config: `{"instance":"` + c.base + `","acct":"Gargron"}`}

	items, cursor, err := c.Fetch(context.Background(), src, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// The two-step flow hits lookup then statuses for the returned id.
	if gotLookupPath != "/api/v1/accounts/lookup" {
		t.Errorf("lookup path = %q, want /api/v1/accounts/lookup", gotLookupPath)
	}
	if gotLookupQuery != "Gargron" {
		t.Errorf("lookup acct = %q, want Gargron", gotLookupQuery)
	}
	if gotStatusesPath != "/api/v1/accounts/1/statuses" {
		t.Errorf("statuses path = %q, want /api/v1/accounts/1/statuses", gotStatusesPath)
	}
	if gotUA == "" {
		t.Error("Mastodon requests should carry a User-Agent header; none sent")
	}
	// The malformed third status is skipped.
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (malformed status skipped)", len(items))
	}
	// Title is the first line of the HTML-stripped content, truncated.
	if items[0].Title != "Hello world!" {
		t.Errorf("item0 title = %q, want %q", items[0].Title, "Hello world!")
	}
	if items[0].URL != "https://mastodon.social/@Gargron/101" {
		t.Errorf("item0 url = %q", items[0].URL)
	}
	// Body is the HTML-stripped plain text: tags gone, entities decoded.
	body := items[0].Body
	if strings.Contains(body, "<") || strings.Contains(body, ">") {
		t.Errorf("body still has HTML tags: %q", body)
	}
	for _, want := range []string{"Hello world!", "Second paragraph."} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
	if !strings.Contains(items[1].Body, "Just a & short note") {
		t.Errorf("item1 body = %q, want decoded entity", items[1].Body)
	}
	// Cursor is non-empty after a successful first poll.
	if cursor == "" {
		t.Error("cursor should be set after a fetch")
	}
}

func TestMastodon_MissingConfigErrors(t *testing.T) {
	c := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(lookupJSON))
	})
	if _, _, err := c.Fetch(context.Background(), &kb.Source{Config: `{"acct":"Gargron"}`}, ""); err == nil {
		t.Error("fetch with no configured instance should error")
	}
	if _, _, err := c.Fetch(context.Background(), &kb.Source{Config: `{"instance":"` + c.base + `"}`}, ""); err == nil {
		t.Error("fetch with no configured acct should error")
	}
}

func TestMastodon_FetchCapsItems(t *testing.T) {
	// Build a statuses response larger than the per-fetch cap and confirm it is bounded.
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < maxStatuses+25; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"1","url":"https://mastodon.social/@u/1","content":"<p>hi</p>"}`)
	}
	b.WriteString("]")
	c := testConnector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/accounts/lookup" {
			_, _ = w.Write([]byte(lookupJSON))
			return
		}
		_, _ = w.Write([]byte(b.String()))
	})
	src := &kb.Source{Config: `{"instance":"` + c.base + `","acct":"Gargron"}`}
	items, _, err := c.Fetch(context.Background(), src, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) > maxStatuses {
		t.Errorf("items = %d, want <= cap %d", len(items), maxStatuses)
	}
}

func TestMastodon_HealthCheck(t *testing.T) {
	c := testConnector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/accounts/lookup" {
			_, _ = w.Write([]byte(lookupJSON))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	src := &kb.Source{Config: `{"instance":"` + c.base + `","acct":"Gargron"}`}
	h, err := c.HealthCheck(context.Background(), src)
	if err != nil || !h.OK {
		t.Errorf("healthcheck = %+v, %v, want OK", h, err)
	}

	bad := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	badSrc := &kb.Source{Config: `{"instance":"` + bad.base + `","acct":"Gargron"}`}
	if h, _ := bad.HealthCheck(context.Background(), badSrc); h.OK || !strings.Contains(h.Detail, "status") {
		t.Errorf("unhealthy check = %+v, want not-OK with a status detail", h)
	}
}

func TestMastodon_StripHTML(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"<p>Hello <strong>world</strong>!</p>", "Hello world!"},
		{"<p>a</p><p>b</p>", "a\n\nb"},
		{"line<br>break", "line\nbreak"},
		{"&amp;&lt;&gt;", "&<>"},
		{"no tags", "no tags"},
	}
	for _, tc := range cases {
		if got := stripHTML(tc.in); got != tc.want {
			t.Errorf("stripHTML(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
