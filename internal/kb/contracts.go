package kb

import "context"

// The four plugin contracts are the system's entire extensibility surface
// (design §7): a new source, content type, enrichment step, or output format is
// added by implementing one of these and registering it; the core never changes.
// Most are forward-declared here for Phase 2+; the pipeline package uses Enricher.

// ResourceRef identifies content to fetch (a URL or a local resource).
type ResourceRef struct {
	URL         string
	ContentType string // MIME, when known
}

// Extracted is a Fetcher's output: normalized Markdown plus metadata. Partial
// extraction beats failure (graceful degradation, §8.3).
type Extracted struct {
	Title        string
	BodyMarkdown string
	ContentType  string
	Metadata     map[string]string
	Media        []string // paths/URLs of extracted media
}

// Fetcher turns a ResourceRef into extracted content by type (design §7 / §8.3).
type Fetcher interface {
	CanHandle(ref ResourceRef) bool
	Extract(ctx context.Context, ref ResourceRef) (Extracted, error)
}

// ArtifactPatch is a set of optional artifact-field updates produced by an
// enrichment stage (fetch sets Body/Type; relevance sets RelevanceScore). nil
// fields are left unchanged, so the pipeline applies a narrow UPDATE of only the
// changed columns (keeping cr-sqlite per-column metadata churn minimal, §6.0).
type ArtifactPatch struct {
	Type           *ArtifactType
	BodyMarkdown   *string
	ContentHash    *string
	Status         *ArtifactStatus
	RelevanceScore *float64
}

// EnrichResult is one enricher's output: zero or more enrichment rows plus an
// optional patch to the artifact itself.
type EnrichResult struct {
	Enrichments []Enrichment
	Patch       *ArtifactPatch
}

// Enricher is one stage of the enrichment pipeline DAG (design §7 / §5).
// DependsOn names the stages that must complete first; PrivacyOK lets a
// cloud-backed enricher refuse sensitive/restricted artifacts — the privacy gate
// enforces it before dispatch (§8.17).
type Enricher interface {
	Name() string
	DependsOn() []string
	PrivacyOK(tier PrivacyTier) bool
	Enrich(ctx context.Context, a *Artifact) (EnrichResult, error)
}

// RenderOpts configures a render.
type RenderOpts struct {
	Format  string
	Options map[string]string
}

// Rendered is a Renderer's output.
type Rendered struct {
	Format     string
	OutputPath string
	PrivacyMax PrivacyTier // highest tier included (export filter, §8.14)
}

// Renderer turns artifacts into an output format (design §7, Output/Render layer).
type Renderer interface {
	Format() string
	Render(ctx context.Context, arts []*Artifact, opts RenderOpts) (Rendered, error)
}

// Cursor is an opaque incremental-fetch position for a PULL source.
type Cursor string

// RawItem is one item a source yields before ingestion.
type RawItem struct {
	URL     string
	Body    string
	Title   string
	Context string // JSON: source-specific provenance
}

// Health is a source's health snapshot (design §8.15). It is persisted as JSON
// (the source scheduler) and surfaced by the health endpoint, so the keys are
// stable lowercase.
type Health struct {
	OK        bool   `json:"ok"`
	Detail    string `json:"detail,omitempty"`
	AuthValid bool   `json:"auth_valid,omitempty"`
}

// SourceConnector is the behavioral contract a connector implements (design §7 /
// §8.1). One connector instance serves every Source that uses it (the registry
// maps a connector name to one), so the per-source configuration — feed URL,
// auth, cursor — is passed in via *Source on each call rather than baked into the
// connector.
type SourceConnector interface {
	Name() string     // the connector type, e.g. "rss"
	Kind() SourceKind // PULL | PUSH | AMBIENT
	// Fetch returns the source's new items since the cursor, plus an updated
	// cursor to persist. PULL only.
	Fetch(ctx context.Context, src *Source, since Cursor) ([]RawItem, Cursor, error)
	HealthCheck(ctx context.Context, src *Source) (Health, error)
}
