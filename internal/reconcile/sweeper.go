// Package reconcile is the cross-replica dedup sweep (design §8.2 / §8.16). Under
// cr-sqlite peer sync the database can no longer forbid duplicate artifacts (a
// non-primary-key unique index is not allowed on a replicated table), so two
// offline devices can each create the same URL/content. This sweep finds those
// duplicates and merges each group into one survivor — the "dedup after sync" the
// design's same_as relation describes. It runs single-node too (harmless: there
// are simply no duplicates to merge).
package reconcile

import (
	"context"
	"errors"
	"fmt"

	"kb/internal/store"
)

// Sweeper merges duplicate artifacts over the store.
type Sweeper struct {
	st *store.Store
}

// New wires a Sweeper to the store.
func New(st *store.Store) *Sweeper { return &Sweeper{st: st} }

// Sweep merges every duplicate group into its lowest-id survivor and returns how
// many losers it merged. The survivor is deterministic (the group's lowest id), so
// independent replicas converge on the same merge; Merge is idempotent, so a
// re-run after sync is safe. A per-group failure is collected and the rest proceed.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	groups, err := s.st.Artifacts.DuplicateGroups(ctx)
	if err != nil {
		return 0, err
	}
	merged := 0
	var errs []error
	for _, g := range groups {
		survivor := g[0] // lowest id (groups come id-ascending)
		for _, loser := range g[1:] {
			err := s.st.Tx(ctx, func(tx *store.Store) error {
				return tx.Artifacts.Merge(ctx, survivor, loser)
			})
			if err != nil {
				if ctx.Err() != nil {
					return merged, err
				}
				errs = append(errs, fmt.Errorf("merge %q into %q: %w", loser, survivor, err))
				continue
			}
			merged++
		}
	}
	return merged, errors.Join(errs...)
}
