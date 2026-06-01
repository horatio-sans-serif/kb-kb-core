// Package enrich holds the concrete kb.Enricher implementations that the
// pipeline runs (design §8.5). They are pure plugins over the kb domain types,
// with their external dependencies (a fetcher, an LLM client) injected.
package enrich

import (
	"context"
	"encoding/json"
	"fmt"

	"kb/internal/kb"
)

// Fetch is the first enrichment stage: it retrieves a URL artifact's content and
// writes it to the artifact body (design §8.3). A body artifact (no canonical
// URL) is a no-op. PrivacyOK is always true — fetching is local retrieval, not
// LLM processing; the privacy gate governs the enrichers that consume the body.
type Fetch struct{ fetcher kb.Fetcher }

// NewFetch wraps a fetcher as the "fetch" stage.
func NewFetch(f kb.Fetcher) *Fetch { return &Fetch{fetcher: f} }

func (*Fetch) Name() string                  { return "fetch" }
func (*Fetch) DependsOn() []string           { return nil }
func (*Fetch) PrivacyOK(kb.PrivacyTier) bool { return true }

func (f *Fetch) Enrich(ctx context.Context, a *kb.Artifact) (kb.EnrichResult, error) {
	if a.CanonicalURL == nil {
		return kb.EnrichResult{}, nil // a body artifact has nothing to fetch
	}
	ref := kb.ResourceRef{URL: *a.CanonicalURL}
	if !f.fetcher.CanHandle(ref) {
		return kb.EnrichResult{}, fmt.Errorf("fetch: no fetcher for %s", ref.URL)
	}
	ext, err := f.fetcher.Extract(ctx, ref)
	if err != nil {
		return kb.EnrichResult{}, err
	}
	body := ext.BodyMarkdown
	meta, _ := json.Marshal(map[string]string{"title": ext.Title, "content_type": ext.ContentType})
	return kb.EnrichResult{
		Patch: &kb.ArtifactPatch{BodyMarkdown: &body},
		Enrichments: []kb.Enrichment{
			{ArtifactID: a.ID, Kind: "fetch", Payload: string(meta), Model: "web", PromptVersion: "v1"},
		},
	}, nil
}
