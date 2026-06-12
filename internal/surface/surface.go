// Package surface is the surfacing engine (design §8.11): it selects "what should
// I see now" — the ready, scored artifacts ranked by relevance — as the input to a
// digest render or a review queue. It closes the cyclic loop (§5): surfaced items
// are engaged with, which re-scores them.
package surface

import (
	"context"

	"kb/internal/kb"
	"kb/internal/store"
)

// readyStatus is the only pipeline state a surfaced artifact may be in: it has
// been fetched, enriched, and scored. Still-enriching or failed artifacts are
// never surfaced.
const readyStatus = "ready"

// Options shapes a surfacing query. PrivacyCeiling drops artifacts above the tier;
// it fails closed — an empty or invalid ceiling surfaces nothing, so pass
// kb.Restricted to surface every tier (the owner). Since bounds first_seen_at (new
// arrivals); MinScore floors the relevance score; Limit caps the result.
type Options struct {
	Limit          int
	Since          string
	MinScore       float64
	PrivacyCeiling kb.PrivacyTier
}

// Surfacer ranks artifacts for surfacing over the store.
type Surfacer struct {
	st *store.Store
}

// New wires a Surfacer to the store.
func New(st *store.Store) *Surfacer { return &Surfacer{st: st} }

// Surface returns the ready, scored artifacts ranked by relevance (then recency)
// under opts.
func (s *Surfacer) Surface(ctx context.Context, opts Options) ([]*kb.Artifact, error) {
	return s.st.Artifacts.Surface(ctx, store.SurfaceQuery{
		Status:         readyStatus,
		Since:          opts.Since,
		MinScore:       opts.MinScore,
		PrivacyCeiling: opts.PrivacyCeiling,
		Limit:          opts.Limit,
	})
}
