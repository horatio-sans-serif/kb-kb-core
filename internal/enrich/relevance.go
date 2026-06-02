package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"kb/internal/kb"
	"kb/internal/relevance"
)

const relevancePromptVersion = "v1"

// RelevanceStage is the relevance stage's name — the single source of truth for
// both the enricher and the re-score path that re-enqueues it standalone (§9).
const RelevanceStage = "relevance"

// SignalProvider supplies the cross-artifact signals the relevance stage needs
// beyond the artifact row: its embedding vector, encounter count, and the weight
// of its most authoritative source. A store-backed adapter implements it; the
// enricher stays a pure plugin over kb types.
type SignalProvider interface {
	Signals(ctx context.Context, artifactID string) (relevance.Signals, error)
}

// Relevance scores an artifact against the profile (design §8.9) and writes the
// score to the artifact (a hot-update column, §6.0) plus a `relevance` enrichment
// row carrying the factor breakdown for explainability. It runs after `embed` so
// the embedding it scores against the profile already exists.
//
// PrivacyOK is always true: relevance is a pure local computation over already-
// stored data — nothing leaves the machine, regardless of tier.
type Relevance struct {
	provider SignalProvider
	profile  *relevance.Profile
	now      func() time.Time
}

// NewRelevance builds the "relevance" stage over a signal provider and profile.
func NewRelevance(provider SignalProvider, profile *relevance.Profile) *Relevance {
	return &Relevance{provider: provider, profile: profile, now: time.Now}
}

func (*Relevance) Name() string                  { return RelevanceStage }
func (*Relevance) DependsOn() []string           { return []string{"embed"} }
func (*Relevance) PrivacyOK(kb.PrivacyTier) bool { return true }

func (r *Relevance) Enrich(ctx context.Context, a *kb.Artifact) (kb.EnrichResult, error) {
	sig, err := r.provider.Signals(ctx, a.ID)
	if err != nil {
		return kb.EnrichResult{}, err
	}
	if len(sig.Embedding) == 0 {
		return kb.EnrichResult{}, nil // no embedding (e.g. empty body) — nothing to score
	}
	// A profile/embedding dimension mismatch (e.g. the embed model changed) makes
	// every cosine 0 and silently zeroes the score. Surface it as a stage error so
	// the misconfiguration is visible rather than producing meaningless scores.
	if d := r.profile.Dim(); d != 0 && d != len(sig.Embedding) {
		return kb.EnrichResult{}, fmt.Errorf("relevance: embedding dim %d != profile dim %d (embed model mismatch?)", len(sig.Embedding), d)
	}

	bd := relevance.Score(relevance.Inputs{
		Match:        r.profile.Match(sig.Embedding),
		AgeHours:     r.ageHours(a.LastSeenAt),
		Encounters:   sig.Encounters,
		Engaged:      a.LastEngagedAt != nil,
		SourceWeight: sig.SourceWeight,
	})

	// Marshal explicitly and propagate (consistent with the embed stage): Score
	// sanitizes its inputs so the breakdown is always finite, but never persist a
	// lossy fallback for a score row.
	payload, err := json.Marshal(bd)
	if err != nil {
		return kb.EnrichResult{}, fmt.Errorf("relevance: marshal breakdown: %w", err)
	}
	score := bd.Score
	return kb.EnrichResult{
		Patch: &kb.ArtifactPatch{RelevanceScore: &score},
		Enrichments: []kb.Enrichment{{
			ArtifactID:    a.ID,
			Kind:          "relevance",
			Payload:       string(payload),
			Model:         "relevance/" + relevancePromptVersion,
			PromptVersion: relevancePromptVersion,
		}},
	}, nil
}

// ageHours is the artifact's age in hours from last_seen_at to now. An unparseable
// timestamp is treated as age 0 (fully recent) rather than failing the stage.
func (r *Relevance) ageHours(lastSeen string) float64 {
	t, err := time.Parse(time.RFC3339Nano, lastSeen)
	if err != nil {
		return 0
	}
	d := r.now().Sub(t).Hours()
	if d < 0 {
		return 0
	}
	return d
}
