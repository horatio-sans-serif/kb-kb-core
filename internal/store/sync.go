package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SyncRepo persists per-peer changeset watermarks for multi-device sync (§8.16). The
// puller records how far it has pulled from each peer (the peer's db_version) so the
// next pull transfers only newer changes. Local-only, in the sync_state table.
type SyncRepo struct{ q, rq Querier } // q: write pool; rq: read pool

// Watermark returns the highest db_version pulled from peer, or 0 if never synced
// (or on a fresh watermark) — re-pulling from a lower mark is safe (idempotent
// merge), so an unknown peer simply pulls from the start.
func (r *SyncRepo) Watermark(ctx context.Context, peer string) (int64, error) {
	var v int64
	err := r.rq.QueryRowContext(ctx, `SELECT last_version FROM sync_state WHERE peer = ?`, peer).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read watermark for %q: %w", peer, err)
	}
	return v, nil
}

// SetWatermark records the highest db_version pulled from peer (monotonic: a lower
// value never overwrites a higher one, so an out-of-order apply cannot rewind it).
func (r *SyncRepo) SetWatermark(ctx context.Context, peer string, version int64) error {
	if _, err := r.q.ExecContext(ctx,
		`INSERT INTO sync_state(peer, last_version, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(peer) DO UPDATE SET last_version = MAX(last_version, excluded.last_version), updated_at = excluded.updated_at`,
		peer, version, now()); err != nil {
		return fmt.Errorf("set watermark for %q: %w", peer, err)
	}
	return nil
}
