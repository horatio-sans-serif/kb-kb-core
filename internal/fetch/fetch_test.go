package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kb/internal/kb"
)

func TestWebFetcher_Extract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><head><title>Hi &amp; Bye</title></head>`+
			`<body><script>secret_script_token</script><h1>Heading</h1><p>Hello world.</p></body></html>`)
	}))
	defer srv.Close()

	f := NewWebFetcher(srv.Client())
	got, err := f.Extract(context.Background(), kb.ResourceRef{URL: srv.URL})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Title != "Hi & Bye" {
		t.Errorf("title = %q, want %q (entities decoded)", got.Title, "Hi & Bye")
	}
	if !strings.Contains(got.BodyMarkdown, "Heading") || !strings.Contains(got.BodyMarkdown, "Hello world.") {
		t.Errorf("body = %q, want it to contain the heading and paragraph", got.BodyMarkdown)
	}
	if strings.Contains(got.BodyMarkdown, "secret_script_token") {
		t.Errorf("body should not contain <script> contents: %q", got.BodyMarkdown)
	}
}

func TestWebFetcher_StripsNoscript(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><body><noscript>noscript_token</noscript><p>Visible.</p></body></html>`)
	}))
	defer srv.Close()

	got, err := NewWebFetcher(srv.Client()).Extract(context.Background(), kb.ResourceRef{URL: srv.URL})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if strings.Contains(got.BodyMarkdown, "noscript_token") {
		t.Errorf("body should not contain <noscript> contents: %q", got.BodyMarkdown)
	}
	if !strings.Contains(got.BodyMarkdown, "Visible.") {
		t.Errorf("body = %q, want the visible paragraph", got.BodyMarkdown)
	}
}

func TestWebFetcher_RejectsNonHTMLContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, "%PDF-1.7 not really html")
	}))
	defer srv.Close()
	if _, err := NewWebFetcher(srv.Client()).Extract(context.Background(), kb.ResourceRef{URL: srv.URL}); err == nil {
		t.Error("a non-HTML content type should be rejected (the HTML extractor must not run on binary)")
	}
}

func TestWebFetcher_RejectsOversizedBody(t *testing.T) {
	big := strings.Repeat("a", maxBodyBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()
	if _, err := NewWebFetcher(srv.Client()).Extract(context.Background(), kb.ResourceRef{URL: srv.URL}); err == nil {
		t.Errorf("a body over the %d-byte cap should error, not silently truncate", maxBodyBytes)
	}
}

func TestWebFetcher_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	f := NewWebFetcher(srv.Client())
	if _, err := f.Extract(context.Background(), kb.ResourceRef{URL: srv.URL}); err == nil {
		t.Error("a non-200 response should be an error")
	}
}

func TestWebFetcher_GuardedClientRefusesLoopback(t *testing.T) {
	// The production fetcher's SSRF-guarded dialer must refuse a loopback URL.
	if _, err := New().Extract(context.Background(), kb.ResourceRef{URL: "http://127.0.0.1:1/x"}); err == nil {
		t.Error("guarded fetcher should refuse a loopback URL")
	}
}

func TestWebFetcher_CanHandle(t *testing.T) {
	f := New()
	if !f.CanHandle(kb.ResourceRef{URL: "https://example.com"}) {
		t.Error("should handle https")
	}
	if f.CanHandle(kb.ResourceRef{URL: "file:///etc/passwd"}) {
		t.Error("should not handle file://")
	}
}
