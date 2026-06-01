package store

import (
	"context"
	"fmt"
)

// Encounter is defined in internal/kb (aliased in types.go).
type EncounterRepo struct{ q, rq Querier } // q: write pool; rq: read pool

// Record always inserts a new encounter (never deduped — the artifact is the
// deduped entity, the encounter is the per-sighting fact).
func (r *EncounterRepo) Record(ctx context.Context, e *Encounter) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	ts := now()
	if e.SeenAt == "" {
		e.SeenAt = ts
	}
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO encounters(id, artifact_id, source_id, raw_url, context, seen_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ArtifactID, e.SourceID, e.RawURL, nullIfEmpty(e.Context), e.SeenAt, ts)
	if err != nil {
		return fmt.Errorf("record encounter: %w", err)
	}
	return nil
}

// CountForArtifact returns how many times the artifact has been encountered.
func (r *EncounterRepo) CountForArtifact(ctx context.Context, artifactID string) (int, error) {
	var n int
	if err := r.rq.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM encounters WHERE artifact_id = ?`, artifactID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count encounters for %q: %w", artifactID, err)
	}
	return n, nil
}
