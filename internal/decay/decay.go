// Package decay periodically re-scores artifacts so time-based recency decay
// takes effect (design §8.9). The relevance factors are all live, but the score
// only recomputes on ingest/engage/re-encounter — so without a sweep an old,
// untouched artifact keeps its day-0 score forever. The sweep recomputes the
// score in place; it deliberately writes no enrichment rows, so running it on a
// schedule does not grow the enrichments table.
package decay

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"kb/internal/kb"
	"kb/internal/relevance"
	"kb/internal/relsignals"
	"kb/internal/store"
)

const (
	// maxEngaged bounds the living-profile set loaded once per sweep (mirrors the
	// relsignals cap).
	maxEngaged = 32
	// scoreEpsilon skips a patch when the recomputed score barely moved, so a
	// sweep only writes the artifacts whose relevance actually changed. It freezes
	// only sub-epsilon movement deep in the decay tail (recency already ≈ 0, the
	// artifact already de-ranked), which is intended.
	scoreEpsilon = 1e-4
)

// Sweeper recomputes relevance_score for every live artifact.
type Sweeper struct {
	st      *store.Store
	sigs    *relsignals.Provider
	profile *relevance.Profile
	now     func() time.Time
}

// NewSweeper builds a sweeper over the store and the (static) profile.
func NewSweeper(st *store.Store, profile *relevance.Profile) *Sweeper {
	return &Sweeper{st: st, sigs: relsignals.New(st), profile: profile, now: time.Now}
}

type engagedVec struct {
	id  string
	vec []float32
}

// Sweep re-scores every live artifact, patching relevance_score in place, and
// returns how many it changed. The living-profile engaged set is loaded once (it
// is the same for the whole pass), and a score that barely moved is left as-is.
//
// A per-artifact error (a corrupt embedding, a failed patch) is collected and the
// sweep continues, so one poison row cannot wedge the whole corpus; the joined
// errors are returned alongside the count. Only context cancellation aborts the
// pass. Last-writer-wins against a concurrent event-driven re-score is
// intentional: both compute from the same factors, so any divergence is at most
// one decay step and self-corrects on the next event or sweep.
//
// Memory: List materializes all live artifacts at once — single-digit MB at
// personal scale (tens of thousands). Keyset pagination by ULID would be the move
// only at hundreds of thousands.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	engaged, err := s.engagedSet(ctx)
	if err != nil {
		return 0, err
	}
	arts, err := s.st.Artifacts.List(ctx)
	if err != nil {
		return 0, err
	}

	now := s.now()
	dim := s.profile.Dim()
	changed := 0
	var errs error
	for _, a := range arts {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		sig, err := s.sigs.Signals(ctx, a.ID)
		if err != nil {
			errs = errors.Join(errs, err) // skip this artifact, keep sweeping
			continue
		}
		if len(sig.Embedding) == 0 {
			continue // not yet scored (no embedding) — leave it for the pipeline
		}
		if dim != 0 && dim != len(sig.Embedding) {
			continue // embed-model mismatch — leave the score for a re-enrich to fix
		}
		bd := relevance.Score(relevance.Inputs{
			Match:        s.profile.MatchWith(sig.Embedding, vectorsExcluding(engaged, a.ID)),
			AgeHours:     relevance.AgeHours(a.RecencyAnchor(), now),
			Encounters:   sig.Encounters,
			Engaged:      a.LastEngagedAt != nil,
			SourceWeight: sig.SourceWeight,
		})
		if a.RelevanceScore != nil && math.Abs(*a.RelevanceScore-bd.Score) < scoreEpsilon {
			continue // unchanged — skip the write
		}
		score := bd.Score
		if err := s.st.Artifacts.Patch(ctx, a.ID, store.ArtifactPatch{RelevanceScore: &score}); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		changed++
	}
	return changed, errs
}

func (s *Sweeper) engagedSet(ctx context.Context) ([]engagedVec, error) {
	rows, err := s.st.Enrichments.EngagedEmbeddings(ctx, "", maxEngaged)
	if err != nil {
		return nil, err
	}
	out := make([]engagedVec, 0, len(rows))
	for _, r := range rows {
		var ep kb.EmbeddingPayload
		if json.Unmarshal([]byte(r.Payload), &ep) != nil || len(ep.Vector) == 0 {
			continue
		}
		out = append(out, engagedVec{id: r.ArtifactID, vec: ep.Vector})
	}
	return out, nil
}

// vectorsExcluding returns the engaged vectors minus the artifact being scored, so
// it never matches itself.
func vectorsExcluding(set []engagedVec, excludeID string) [][]float32 {
	out := make([][]float32, 0, len(set))
	for _, e := range set {
		if e.id != excludeID {
			out = append(out, e.vec)
		}
	}
	return out
}
