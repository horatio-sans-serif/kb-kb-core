// Package kb holds the pure domain model and plugin contracts — the types that
// connectors, fetchers, enrichers, and renderers traffic in. It depends on no
// other internal package (and not on database/sql), so plugins can be built
// against it without importing the storage layer (design §7).
package kb

// PrivacyTier classifies how sensitive an artifact is (design §8.17).
type PrivacyTier string

const (
	Public     PrivacyTier = "public"
	Personal   PrivacyTier = "personal"
	Sensitive  PrivacyTier = "sensitive"
	Restricted PrivacyTier = "restricted"
)

// Valid reports whether t is a known tier.
func (t PrivacyTier) Valid() bool {
	switch t {
	case Public, Personal, Sensitive, Restricted:
		return true
	}
	return false
}

// AllowsCloud reports whether an artifact at this tier may be processed by a
// cloud (non-local) service. Sensitive and restricted content must stay local
// (design Principle 7); this is the floor the privacy gate enforces.
func (t PrivacyTier) AllowsCloud() bool {
	return t == Public || t == Personal
}

// SourceKind is how a source delivers content (design §7).
type SourceKind string

const (
	Pull    SourceKind = "PULL"    // KB fetches on a schedule
	Push    SourceKind = "PUSH"    // the user sends content in
	Ambient SourceKind = "AMBIENT" // passive background capture
)

// ArtifactType is the kind of content an artifact holds (design §6.2).
type ArtifactType string

const (
	Webpage    ArtifactType = "webpage"
	PDF        ArtifactType = "pdf"
	Video      ArtifactType = "video"
	Audio      ArtifactType = "audio"
	Image      ArtifactType = "image"
	Note       ArtifactType = "note"
	Transcript ArtifactType = "transcript"
	Message    ArtifactType = "message"
	SocialPost ArtifactType = "social_post"
	Email      ArtifactType = "email"
	Code       ArtifactType = "code"
	Render     ArtifactType = "render"
)

// LifecycleTier is where an artifact sits in the active/archive/purge lifecycle.
type LifecycleTier string

const (
	Active  LifecycleTier = "active"
	Archive LifecycleTier = "archive"
	Purged  LifecycleTier = "purged"
)

// ArtifactStatus is the enrichment-pipeline state of an artifact.
type ArtifactStatus string

const (
	StatusIngested  ArtifactStatus = "ingested"
	StatusEnriching ArtifactStatus = "enriching"
	StatusReady     ArtifactStatus = "ready"
	StatusFailed    ArtifactStatus = "failed"
)

// Artifact is a canonical knowledge item (design §6.2). Nullable columns are
// pointers so SQL NULL round-trips; timestamps are ISO-8601 strings (audit
// timestamps, not logical merge clocks — see §6.0).
type Artifact struct {
	ID             string
	Type           ArtifactType
	CanonicalURL   *string
	ContentHash    *string
	Title          string
	BodyMarkdown   string
	PrivacyTier    PrivacyTier
	LifecycleTier  LifecycleTier
	Status         ArtifactStatus
	RelevanceScore *float64
	FirstSeenAt    string
	LastSeenAt     string
	LastEngagedAt  *string
	DeletedAt      *string
	CreatedAt      string
	UpdatedAt      string
}

// RecencyAnchor is the timestamp the relevance recency factor decays from: the
// last engagement if any, else when the artifact first entered the KB. It is
// deliberately NOT last_seen_at — a re-sighting from a feed must not reset
// user-staleness (that is the encounter factor's signal, §8.9), so a stale,
// unengaged artifact keeps decaying even while it is still being re-sighted.
func (a *Artifact) RecencyAnchor() string {
	if a.LastEngagedAt != nil {
		return *a.LastEngagedAt
	}
	return a.FirstSeenAt
}

// Encounter records one sighting of an artifact from one source (design §6.3).
// Encounters accumulate; their count is itself a relevance signal (§8.9).
type Encounter struct {
	ID         string
	ArtifactID string
	SourceID   string
	RawURL     *string
	Context    string // JSON; stored NULL when empty
	SeenAt     string
	CreatedAt  string
}

// Enrichment is a versioned output of one pipeline stage for one artifact
// (design §6.4). Embedding vectors travel here as payload data so every replica
// can rebuild its local ANN index (§6.0).
type Enrichment struct {
	ID            string
	ArtifactID    string
	Kind          string // summary | tags | critical_analysis | relevance | embedding | ...
	Payload       string // JSON
	Model         string
	PromptVersion string
	CreatedAt     string
}

// EmbeddingPayload is the JSON shape of an "embedding" enrichment row's payload.
// Dim is stored alongside the vector so a reader can validate it against a local
// index's fixed dimension (§6.5). The embed stage writes it; the relevance stage
// (via its signal provider) reads it back.
type EmbeddingPayload struct {
	Model  string    `json:"model"`
	Dim    int       `json:"dim"`
	Vector []float32 `json:"vector"`
}

// Source is a registered origin of artifacts (design §6.1). Behavioral source
// connectors implement the Source interface in contracts.go; this is the stored
// configuration row.
type Source struct {
	ID             string
	Kind           SourceKind
	Connector      string
	Config         string  // JSON
	Schedule       *string // JSON refresh policy (cron|ttl|rrule); Phase 4
	AuthState      *string // JSON token/oauth state + expiry; Phase 4
	DefaultPrivacy PrivacyTier
	StorageBudget  *int
	SourceWeight   float64
	Enabled        bool
	CreatedAt      string
	// PULL fetch state (§8.1 / §8.15), updated by the scheduler.
	Cursor        *string // opaque incremental-fetch position
	LastFetchedAt *string // last successful (or attempted) poll
	Health        *string // JSON Health snapshot from the last poll
}
