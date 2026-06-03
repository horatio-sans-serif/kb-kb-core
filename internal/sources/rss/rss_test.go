package rss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"kb/internal/kb"
)

const rssXML = `<?xml version="1.0"?>
<rss version="2.0"><channel>
  <title>Feed</title>
  <item><title>Newer</title><link>https://e.com/2</link><description>d2</description><pubDate>Tue, 02 Jun 2026 10:00:00 +0000</pubDate></item>
  <item><title>Older</title><link>https://e.com/1</link><description>d1</description><pubDate>Mon, 01 Jun 2026 10:00:00 +0000</pubDate></item>
</channel></rss>`

const atomXML = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Feed</title>
  <entry><title>A1</title><link href="https://e.com/a1" rel="alternate"/><id>urn:1</id><updated>2026-06-02T10:00:00Z</updated><summary>s1</summary></entry>
</feed>`

func serve(t *testing.T, body string) *kb.Source {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	cfg := `{"url":"` + srv.URL + `"}`
	return &kb.Source{ID: "01SRC", Config: cfg}
}

func TestFetch_RSS(t *testing.T) {
	c := New(http.DefaultClient)
	src := serve(t, rssXML)

	items, cursor, err := c.Fetch(context.Background(), src, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	// Item with the article link carries the URL for the fetch stage.
	got := map[string]string{}
	for _, it := range items {
		got[it.Title] = it.URL
	}
	if got["Newer"] != "https://e.com/2" || got["Older"] != "https://e.com/1" {
		t.Errorf("items = %+v, want links mapped to titles", got)
	}
	if cursor != "2026-06-02T10:00:00Z" {
		t.Errorf("cursor = %q, want the newest item's time", cursor)
	}

	// A second fetch with that cursor returns nothing new.
	items2, _, err := c.Fetch(context.Background(), src, cursor)
	if err != nil {
		t.Fatalf("fetch 2: %v", err)
	}
	if len(items2) != 0 {
		t.Errorf("incremental fetch returned %d items, want 0", len(items2))
	}
}

func TestFetch_Atom(t *testing.T) {
	c := New(http.DefaultClient)
	src := serve(t, atomXML)
	items, cursor, err := c.Fetch(context.Background(), src, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 1 || items[0].URL != "https://e.com/a1" || items[0].Title != "A1" {
		t.Errorf("items = %+v, want one Atom entry with its alternate link", items)
	}
	if cursor != "2026-06-02T10:00:00Z" {
		t.Errorf("cursor = %q, want the entry's updated time", cursor)
	}
}

func TestFetch_Incremental(t *testing.T) {
	c := New(http.DefaultClient)
	src := serve(t, rssXML)
	// Cursor at the older item's time → only the newer item is returned.
	items, _, err := c.Fetch(context.Background(), src, "2026-06-01T10:00:00Z")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Newer" {
		t.Errorf("items = %+v, want only the newer item", items)
	}
}

func TestFetch_MissingURLConfig(t *testing.T) {
	c := New(http.DefaultClient)
	if _, _, err := c.Fetch(context.Background(), &kb.Source{Config: `{}`}, ""); err == nil {
		t.Error("a config without a url should error")
	}
}

func TestConnector_Contract(t *testing.T) {
	c := New(http.DefaultClient)
	if c.Name() != "rss" || c.Kind() != kb.Pull {
		t.Errorf("name=%q kind=%q, want rss/PULL", c.Name(), c.Kind())
	}
}
