// Package lifecycle is the artifact lifecycle / garbage-collection engine (design
// §8.12). It transitions artifacts active -> archive -> purge by relevance and
// age: an artifact that has decayed below the archive score and gone untouched
// long enough is archived; one below the purge score and untouched for months is
// purged (tombstoned). Engagement protects an artifact — its recency anchor resets
// on engagement, so age is measured from the last interaction, not first sighting.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"kb/internal/kb"
	"kb/internal/store"
)

// Policy is the lifecycle threshold set. A demotion needs BOTH a low score and
// sufficient age — neither alone suffices.
type Policy struct {
	ArchiveAfter time.Duration // min age (with score < ArchiveBelow) to archive
	PurgeAfter   time.Duration // min age (with score < PurgeBelow) to purge
	ArchiveBelow float64       // score below which (and old enough) an artifact archives
	PurgeBelow   float64       // score below which (and old enough) an artifact purges
}

// DefaultPolicy is the shipped lifecycle policy.
func DefaultPolicy() Policy {
	return Policy{
		ArchiveAfter: 30 * 24 * time.Hour,
		PurgeAfter:   180 * 24 * time.Hour,
		ArchiveBelow: 0.2,
		PurgeBelow:   0.05,
	}
}

// classify returns the lifecycle tier an artifact with the given relevance score
// and age (since its recency anchor) should be in. Higher relevance or younger age
// keeps it active; only a low score AND sufficient age demote it.
func classify(score float64, age time.Duration, p Policy) kb.LifecycleTier {
	if score < p.PurgeBelow && age >= p.PurgeAfter {
		return kb.Purged
	}
	if score < p.ArchiveBelow && age >= p.ArchiveAfter {
		return kb.Archive
	}
	return kb.Active
}

// Stats reports what one sweep changed.
type Stats struct {
	Archived int
	Purged   int
}

// tierRank orders the lifecycle tiers so the sweep can demote (rank increases)
// but never promote: active(0) -> archive(1) -> purged(2).
func tierRank(t kb.LifecycleTier) int {
	switch t {
	case kb.Archive:
		return 1
	case kb.Purged:
		return 2
	default:
		return 0 // active (and any unexpected value) is the lowest
	}
}

// Sweeper applies the lifecycle policy over the store.
type Sweeper struct {
	st     *store.Store
	policy Policy
	now    func() time.Time
}

// New returns a Sweeper with the default policy and wall clock.
func New(st *store.Store) *Sweeper { return NewWithPolicy(st, DefaultPolicy(), time.Now) }

// NewWithPolicy is New with an explicit policy and clock (tests).
func NewWithPolicy(st *store.Store, policy Policy, now func() time.Time) *Sweeper {
	return &Sweeper{st: st, policy: policy, now: now}
}

// Sweep classifies every live, scored artifact and applies any tier change. A
// purge tombstones the artifact (and records the purged tier); other transitions
// patch the tier. A per-artifact failure is collected and the rest proceed.
func (s *Sweeper) Sweep(ctx context.Context) (Stats, error) {
	arts, err := s.st.Artifacts.List(ctx)
	if err != nil {
		return Stats{}, err
	}
	now := s.now()
	var stats Stats
	var errs []error
	for _, a := range arts {
		if a.RelevanceScore == nil {
			continue // unscored artifacts are not subject to relevance-based GC
		}
		anchor, err := time.Parse(store.TimeFormat, a.RecencyAnchor())
		if err != nil {
			continue // unparseable timestamp: leave it alone
		}
		target := classify(*a.RelevanceScore, now.Sub(anchor), s.policy)
		// Demote-only: the sweep moves an artifact down the lifecycle (active ->
		// archive -> purge) but NEVER promotes it back up on its own. Promotion is
		// reserved for an explicit engagement (which resets the artifact to active).
		// This keeps an archive sticky and stops it flapping against a budget archive
		// when a decayed artifact's score later recovers (relevance is not monotonic).
		if tierRank(target) <= tierRank(a.LifecycleTier) {
			continue
		}
		if err := s.apply(ctx, a.ID, target); err != nil {
			if ctx.Err() != nil {
				return stats, err
			}
			errs = append(errs, fmt.Errorf("lifecycle %q: %w", a.ID, err))
			continue
		}
		switch target {
		case kb.Archive:
			stats.Archived++
		case kb.Purged:
			stats.Purged++
		}
	}
	return stats, errors.Join(errs...)
}

// EnforceBudgets archives the lowest-relevance active artifacts of any source that
// is over its storage budget, so a noisy feed can't dominate the active set
// (§8.12). Archiving (not purging) keeps the content but out of the active
// footprint, and the count is of active-tier artifacts, so the budget is a cap on
// how many of a source's artifacts stay active at once. Returns how many it
// archived.
//
// Budgets are active-footprint caps, not ownership: an artifact encountered from
// several sources counts against each, so any over-budget source may archive a
// shared artifact (sources are processed oldest-first, so the first over-budget
// one wins). That is acceptable because archiving is reversible — engagement
// returns the artifact to active — and the lowest-relevance ones are demoted first.
func (s *Sweeper) EnforceBudgets(ctx context.Context) (int, error) {
	srcs, err := s.st.Sources.ListWithBudget(ctx)
	if err != nil {
		return 0, err
	}
	archived := 0
	var errs []error
	for _, src := range srcs {
		if src.StorageBudget == nil {
			continue
		}
		count, err := s.st.Encounters.ActiveArtifactCountForSource(ctx, src.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		over := count - *src.StorageBudget
		if over <= 0 {
			continue
		}
		ids, err := s.st.Encounters.LowestRelevanceActiveForSource(ctx, src.ID, over)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, id := range ids {
			if err := s.apply(ctx, id, kb.Archive); err != nil {
				if ctx.Err() != nil {
					return archived, err
				}
				errs = append(errs, fmt.Errorf("budget archive %q: %w", id, err))
				continue
			}
			archived++
		}
	}
	return archived, errors.Join(errs...)
}

// apply sets an artifact's lifecycle tier; a purge also tombstones it (the tier is
// patched first, while the row is still live, then the soft-delete lands — both in
// one transaction).
func (s *Sweeper) apply(ctx context.Context, id string, target kb.LifecycleTier) error {
	return s.st.Tx(ctx, func(tx *store.Store) error {
		if err := tx.Artifacts.Patch(ctx, id, kb.ArtifactPatch{LifecycleTier: &target}); err != nil {
			return err
		}
		if target == kb.Purged {
			return tx.Artifacts.SoftDelete(ctx, id)
		}
		return nil
	})
}
