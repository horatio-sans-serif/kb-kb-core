package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Encounter records one sighting of an artifact from one source (design 6.3).
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

type EncounterRepo struct{ db *sql.DB }

// Record always inserts a new encounter (never deduped — the artifact is the
// deduped entity, the encounter is the per-sighting fact).
func (r *EncounterRepo) Record(ctx context.Context, e *Encounter) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.SeenAt == "" {
		e.SeenAt = now()
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO encounters(id, artifact_id, source_id, raw_url, context, seen_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.ArtifactID, e.SourceID, e.RawURL, nullIfEmpty(e.Context), e.SeenAt)
	if err != nil {
		return fmt.Errorf("record encounter: %w", err)
	}
	return nil
}

// CountForArtifact returns how many times the artifact has been encountered.
func (r *EncounterRepo) CountForArtifact(ctx context.Context, artifactID string) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM encounters WHERE artifact_id = ?`, artifactID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count encounters for %q: %w", artifactID, err)
	}
	return n, nil
}
