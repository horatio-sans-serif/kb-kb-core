package hn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"kb/internal/kb"
)

const algoliaJSON = `{"hits":[
  {"objectID":"3","title":"High","url":"https://ex.com/high","points":200,"created_at_i":1700000300,"story_text":""},
  {"objectID":"2","title":"Ask HN: text post","url":null,"points":50,"created_at_i":1700000200,"story_text":"some question"},
  {"objectID":"1","title":"Low","url":"https://ex.com/low","points":3,"created_at_i":1700000100,"story_text":""}
]}`

func testConnector(t *testing.T, handler http.HandlerFunc) (*Connector, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &Connector{client: ts.Client(), base: ts.URL}, ts
}

func TestHN_Contract(t *testing.T) {
	c := &Connector{}
	if c.Name() != "hackernews" || c.Kind() != kb.Pull {
		t.Errorf("contract = %q/%q", c.Name(), c.Kind())
	}
}

func TestHN_FetchEmitsItems(t *testing.T) {
	var gotQuery url.Values
	c, _ := testConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(algoliaJSON))
	})
	src := &kb.Source{Config: `{"tags":"story"}`}

	items, cursor, err := c.Fetch(context.Background(), src, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotQuery.Get("tags") != "story" {
		t.Errorf("tags param = %q, want story", gotQuery.Get("tags"))
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (first poll emits all)", len(items))
	}
	// A linked story carries its URL; the text post carries its body.
	if items[0].URL != "https://ex.com/high" || items[0].Title != "High" {
		t.Errorf("item0 = %+v", items[0])
	}
	if items[1].Body != "some question" || items[1].URL != "" {
		t.Errorf("text post should carry its body, got %+v", items[1])
	}
	// Cursor advances to the newest created_at_i.
	if cursor != "1700000300" {
		t.Errorf("cursor = %q, want the newest timestamp 1700000300", cursor)
	}
}

func TestHN_MinPointsFilters(t *testing.T) {
	c, _ := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(algoliaJSON))
	})
	src := &kb.Source{Config: `{"min_points":100}`}
	items, _, err := c.Fetch(context.Background(), src, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 1 || items[0].Title != "High" {
		t.Errorf("min_points 100 should keep only High, got %d items", len(items))
	}
}

func TestHN_CursorFiltersSeen(t *testing.T) {
	c, _ := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(algoliaJSON))
	})
	// Cursor at the middle item: only items at/after it (inclusive boundary).
	items, _, err := c.Fetch(context.Background(), &kb.Source{}, "1700000200")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("cursor 1700000200 should keep the 2 items >= it, got %d", len(items))
	}
}

func TestHN_CursorAdvancesPastSubThresholdNewest(t *testing.T) {
	// The newest item is below min_points (dropped), but the cursor must still
	// advance past it — otherwise it pins the cursor and the window re-emits forever.
	json := `{"hits":[
	  {"objectID":"9","title":"New low","url":"https://ex.com/n","points":5,"created_at_i":1700000900},
	  {"objectID":"8","title":"Old high","url":"https://ex.com/o","points":200,"created_at_i":1700000100}
	]}`
	c, _ := testConnector(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(json)) })
	items, cursor, err := c.Fetch(context.Background(), &kb.Source{Config: `{"min_points":100}`}, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Old high" {
		t.Errorf("items = %+v, want only the supra-threshold one", items)
	}
	if cursor != "1700000900" {
		t.Errorf("cursor = %q, want 1700000900 (advanced past the dropped newest item)", cursor)
	}
}

func TestHN_DiscussionFallback(t *testing.T) {
	json := `{"hits":[{"objectID":"42","title":"Discussion only","url":null,"points":10,"created_at_i":1700000500,"story_text":""}]}`
	c, _ := testConnector(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(json)) })
	items, _, err := c.Fetch(context.Background(), &kb.Source{}, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 1 || items[0].URL != "https://news.ycombinator.com/item?id=42" {
		t.Errorf("link-and-text-less item should fall back to the HN discussion URL, got %+v", items)
	}
}

func TestHN_NoNewItemsKeepsCursor(t *testing.T) {
	c, _ := testConnector(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(algoliaJSON)) })
	// Cursor past every item: nothing new, cursor unchanged.
	items, cursor, err := c.Fetch(context.Background(), &kb.Source{}, "1700009999")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 0 || cursor != "1700009999" {
		t.Errorf("no-new-items = (%d items, cursor %q), want (0, unchanged)", len(items), cursor)
	}
}

func TestHN_DefaultTags(t *testing.T) {
	var gotQuery url.Values
	c, _ := testConnector(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"hits":[]}`))
	})
	if _, _, err := c.Fetch(context.Background(), &kb.Source{Config: "{}"}, ""); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotQuery.Get("tags") != "front_page" {
		t.Errorf("default tags = %q, want front_page", gotQuery.Get("tags"))
	}
}

func TestHN_HealthCheck(t *testing.T) {
	c, _ := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hits":[]}`))
	})
	h, err := c.HealthCheck(context.Background(), &kb.Source{Config: "{}"})
	if err != nil || !h.OK {
		t.Errorf("healthcheck = %+v, %v, want OK", h, err)
	}

	bad, _ := testConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if h, _ := bad.HealthCheck(context.Background(), &kb.Source{Config: "{}"}); h.OK || !strings.Contains(h.Detail, "status") {
		t.Errorf("unhealthy check = %+v, want not-OK with a status detail", h)
	}
}
