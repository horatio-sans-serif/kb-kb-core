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

// Rank orders tiers by sensitivity (public 0 < personal 1 < sensitive 2 <
// restricted 3). The export/render filter (§8.14) and privacy_max are defined in
// terms of this order. An unknown tier ranks above everything, so it is excluded
// by any ceiling and never silently treated as public.
func (t PrivacyTier) Rank() int {
	switch t {
	case Public:
		return 0
	case Personal:
		return 1
	case Sensitive:
		return 2
	case Restricted:
		return 3
	default:
		return 4
	}
}

// AtMost reports whether t is no more sensitive than ceiling — the per-artifact
// predicate the render export filter applies before an artifact is included.
func (t PrivacyTier) AtMost(ceiling PrivacyTier) bool {
	return t.Rank() <= ceiling.Rank()
}

// MaxTier returns the most sensitive of the given tiers, or Public for none — the
// privacy_max a render records (the highest tier of any included artifact, §8.14).
func MaxTier(tiers ...PrivacyTier) PrivacyTier {
	max := Public
	for _, t := range tiers {
		if t.Rank() > max.Rank() {
			max = t
		}
	}
	return max
}

// allTiers is the tier set in ascending sensitivity — the basis for a read ceiling.
var allTiers = []PrivacyTier{Public, Personal, Sensitive, Restricted}

// TiersAtMost returns the tiers no more sensitive than ceiling, ascending — the
// whitelist a read query applies so a lower-trust reader never sees content above
// its ceiling (§8.17). An owner ceiling of Restricted returns all four; Public
// returns only Public. An unknown ceiling returns nothing (fail closed).
func TiersAtMost(ceiling PrivacyTier) []PrivacyTier {
	if !ceiling.Valid() {
		return nil // fail closed: an unknown ceiling sees nothing, never everything
	}
	out := make([]PrivacyTier, 0, len(allTiers))
	for _, t := range allTiers {
		if t.AtMost(ceiling) {
			out = append(out, t)
		}
	}
	return out
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

// RenderRecord is a persisted record of one output the render layer produced
// (design §6.11 / §8.14). It is a provenance/audit row, not the bytes
// themselves: OutputPath points at the rendered file on disk. PrivacyMax is the
// most sensitive tier of any included artifact — the export-filter ceiling
// actually realized, recorded so a later reader can reason about where the file
// may go. (The "render" ArtifactType is a distinct concept: a rendered output
// re-ingested as a knowledge artifact in the cyclic model, §2.)
type RenderRecord struct {
	ID          string
	Format      string
	SourceQuery string   // the query/description that selected the artifacts (optional)
	ArtifactIDs []string // the included artifacts, in render order
	OutputPath  string
	PrivacyMax  PrivacyTier
	CreatedAt   string
}

// Topic is an emergent cluster of semantically-related artifacts (design §6.6 /
// §8.7). Centroid is the running mean of its members' embeddings — the key the
// clustering compares a new artifact against.
type Topic struct {
	ID       string
	Label    string
	Centroid []float32
}

// Claim is a single factual assertion extracted from an artifact (design §6.10 /
// §8.13). Its embedding bounds the contradiction-comparison set (vector
// pre-filter) before the LLM judges whether two claims conflict.
type Claim struct {
	ID         string
	ArtifactID string
	Statement  string
	Embedding  []float32
	CreatedAt  string
}

// Conflict records that two claims contradict each other (design §6.10). It is
// surfaced for human resolution rather than silently storing both; Status moves
// open → resolved|dismissed.
type Conflict struct {
	ID        string
	ClaimA    string
	ClaimB    string
	Status    string // open|resolved|dismissed
	Rationale string
	CreatedAt string
}

// Entity is a node in the knowledge graph (design §6.6 / §8.7): a person, org,
// technology, concept, or place mentioned across artifacts. Deduped by (Kind,
// Name); artifacts link to it via artifact_entities.
type Entity struct {
	ID      string
	Kind    string // person|org|technology|concept|place
	Name    string
	Aliases []string
}

// Link is a directed edge between two artifacts in the knowledge graph (§6.6).
// Relation is one of references|shares_entity|shares_topic|same_as|derived_from.
// Backlinks are the reverse-direction query.
type Link struct {
	ID       string
	SrcID    string
	DstID    string
	Relation string
	Weight   float64
}

// ValidEntityKind reports whether k is a known entity kind (matching the schema
// CHECK), so the graph writer can reject out-of-vocabulary LLM output with a clear
// error rather than a raw constraint failure.
func ValidEntityKind(k string) bool {
	switch k {
	case "person", "org", "technology", "concept", "place":
		return true
	}
	return false
}

// ValidRelation reports whether rel is a known link relation (matching the schema
// CHECK).
func ValidRelation(rel string) bool {
	switch rel {
	case "references", "shares_entity", "shares_topic", "same_as", "derived_from":
		return true
	}
	return false
}

// CanonicalPair orders two artifact ids so an unordered pair always maps to the
// same directed (src, dst) edge. Both the entity and topic stages use it so a
// shared link discovered from either artifact dedups against the links unique
// index — the one home for the graph-direction contract.
func CanonicalPair(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// ReviewState is an artifact's spaced-repetition schedule (design §6.9): an
// SM-2 record of when it is next due, the current interval and ease factor, and
// how many successful reviews it has had. One row per artifact (artifact_id PK).
type ReviewState struct {
	ArtifactID   string
	NextReviewAt string
	IntervalDays float64
	Ease         float64
	Reviews      int
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
