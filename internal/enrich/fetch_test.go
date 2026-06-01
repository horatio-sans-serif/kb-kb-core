package enrich

import (
	"context"
	"errors"
	"testing"

	"kb/internal/kb"
)

type fakeFetcher struct {
	ext    kb.Extracted
	err    error
	handle bool
}

func (f fakeFetcher) CanHandle(kb.ResourceRef) bool { return f.handle }
func (f fakeFetcher) Extract(context.Context, kb.ResourceRef) (kb.Extracted, error) {
	return f.ext, f.err
}

func TestFetch_Enrich(t *testing.T) {
	url := "https://example.com/x"
	a := &kb.Artifact{ID: "01A", CanonicalURL: &url}
	f := NewFetch(fakeFetcher{handle: true, ext: kb.Extracted{Title: "T", BodyMarkdown: "# body", ContentType: "text/html"}})

	res, err := f.Enrich(context.Background(), a)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Patch == nil || res.Patch.BodyMarkdown == nil || *res.Patch.BodyMarkdown != "# body" {
		t.Errorf("patch body = %v, want '# body'", res.Patch)
	}
	if len(res.Enrichments) != 1 || res.Enrichments[0].Kind != "fetch" {
		t.Errorf("enrichments = %v, want one 'fetch' row", res.Enrichments)
	}
}

func TestFetch_BodyArtifactIsNoop(t *testing.T) {
	f := NewFetch(fakeFetcher{handle: true})
	res, err := f.Enrich(context.Background(), &kb.Artifact{ID: "01B"}) // no CanonicalURL
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if res.Patch != nil || len(res.Enrichments) != 0 {
		t.Errorf("a body artifact should be a no-op, got %+v", res)
	}
}

func TestFetch_ExtractErrorPropagates(t *testing.T) {
	url := "https://example.com/x"
	f := NewFetch(fakeFetcher{handle: true, err: errors.New("boom")})
	if _, err := f.Enrich(context.Background(), &kb.Artifact{ID: "01A", CanonicalURL: &url}); err == nil {
		t.Error("a fetch error should propagate")
	}
}
