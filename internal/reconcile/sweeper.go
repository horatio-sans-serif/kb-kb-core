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

// RescoreFunc re-enqueues an artifact's relevance scoring within the caller's
// transaction (the same closure the ingest re-encounter path uses). After a merge
// the survivor inherits the loser's encounters, so its relevance inputs (distinct
// source count, max source weight) changed and the stored score is stale. nil = no
// re-score (single-node setups without a pipeline).
type RescoreFunc func(ctx context.Context, tx *store.Store, artifactID string) error

// Sweeper merges duplicate artifacts over the store.
type Sweeper struct {
	st      *store.Store
	rescore RescoreFunc
}

// New wires a Sweeper to the store. rescore may be nil (no re-score).
func New(st *store.Store, rescore RescoreFunc) *Sweeper {
	return &Sweeper{st: st, rescore: rescore}
}

// Sweep merges every duplicate group into its lowest-id survivor and returns how
// many losers it merged. The survivor is deterministic (the group's lowest id), so
// independent replicas converge on the same survivor. Each merge and the survivor's
// re-score run in one transaction. A per-group failure is collected and the rest
// proceed.
//
// Idempotency: a re-run on a single replica is exact (an already-merged loser is a
// no-op). Concurrent merges on two replicas that then sync converge on the same
// survivor and tombstone, but the child-row DELETEs may leave a residual duplicate
// child (see Merge) — best-effort under §8.16, reconciled exactly by the next
// single-replica sweep.
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
				if err := tx.Artifacts.Merge(ctx, survivor, loser); err != nil {
					return err
				}
				if s.rescore != nil {
					return s.rescore(ctx, tx, survivor)
				}
				return nil
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("merge %q into %q: %w", loser, survivor, err))
				if ctx.Err() != nil {
					return merged, errors.Join(errs...)
				}
				continue
			}
			merged++
		}
	}
	return merged, errors.Join(errs...)
}
