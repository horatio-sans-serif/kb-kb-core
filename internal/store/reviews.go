package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReviewState is defined in internal/kb (aliased in types.go).
type ReviewRepo struct{ q, rq Querier } // q: write pool; rq: read pool

const reviewCols = `artifact_id, next_review_at, interval_days, ease, reviews`

// Upsert writes the schedule for an artifact, inserting or replacing the single
// row keyed by artifact_id. The artifact must exist (FK).
func (r *ReviewRepo) Upsert(ctx context.Context, rs *ReviewState) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO review_schedule(`+reviewCols+`) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(artifact_id) DO UPDATE SET
		   next_review_at = excluded.next_review_at,
		   interval_days  = excluded.interval_days,
		   ease           = excluded.ease,
		   reviews        = excluded.reviews`,
		rs.ArtifactID, rs.NextReviewAt, rs.IntervalDays, rs.Ease, rs.Reviews)
	if err != nil {
		return fmt.Errorf("upsert review %q: %w", rs.ArtifactID, err)
	}
	return nil
}

// Get returns the schedule for an artifact, or ErrNotFound if it has never been
// reviewed.
func (r *ReviewRepo) Get(ctx context.Context, artifactID string) (*ReviewState, error) {
	rs, err := scanReview(r.rq.QueryRowContext(ctx, `SELECT `+reviewCols+` FROM review_schedule WHERE artifact_id = ?`, artifactID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get review %q: %w", artifactID, ErrNotFound)
		}
		return nil, fmt.Errorf("get review %q: %w", artifactID, err)
	}
	return rs, nil
}

// Due returns the schedules whose next_review_at is at or before now, soonest
// first — the review queue. now is a store.TimeFormat string (lexicographic ==
// chronological).
func (r *ReviewRepo) Due(ctx context.Context, now string, limit int) ([]*ReviewState, error) {
	if limit <= 0 {
		limit = surfaceDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT `+reviewCols+` FROM review_schedule
		 WHERE next_review_at <= ? ORDER BY next_review_at, artifact_id LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("due reviews: %w", err)
	}
	defer rows.Close()

	var out []*ReviewState
	for rows.Next() {
		rs, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	return out, rows.Err()
}

func scanReview(sc scanner) (*ReviewState, error) {
	var rs ReviewState
	if err := sc.Scan(&rs.ArtifactID, &rs.NextReviewAt, &rs.IntervalDays, &rs.Ease, &rs.Reviews); err != nil {
		return nil, err
	}
	return &rs, nil
}
