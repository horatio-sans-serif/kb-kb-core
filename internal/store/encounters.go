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

// CountForArtifact returns how many times the artifact has been encountered (the
// total sighting count, including repeats from the same source).
func (r *EncounterRepo) CountForArtifact(ctx context.Context, artifactID string) (int, error) {
	var n int
	if err := r.rq.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM encounters WHERE artifact_id = ?`, artifactID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count encounters for %q: %w", artifactID, err)
	}
	return n, nil
}

// DistinctSourceCountForArtifact returns how many distinct sources have
// encountered the artifact — the "seen from N sources" relevance signal (§8.9).
// Unlike CountForArtifact this does not inflate when one source (e.g. a polled
// feed) re-encounters the same artifact across refreshes.
func (r *EncounterRepo) DistinctSourceCountForArtifact(ctx context.Context, artifactID string) (int, error) {
	var n int
	if err := r.rq.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT source_id) FROM encounters WHERE artifact_id = ?`, artifactID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count distinct sources for %q: %w", artifactID, err)
	}
	return n, nil
}

// ActiveArtifactCountForSource counts the distinct live, active-tier, scored
// artifacts encountered from a source — the footprint a storage budget caps.
func (r *EncounterRepo) ActiveArtifactCountForSource(ctx context.Context, sourceID string) (int, error) {
	var n int
	err := r.rq.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT e.artifact_id) FROM encounters e
		 JOIN artifacts a ON a.id = e.artifact_id
		   AND a.deleted_at IS NULL AND a.lifecycle_tier = 'active' AND a.relevance_score IS NOT NULL
		 WHERE e.source_id = ?`, sourceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("active artifact count for source %q: %w", sourceID, err)
	}
	return n, nil
}

// LowestRelevanceActiveForSource returns up to limit distinct live, active-tier,
// scored artifact ids encountered from a source, lowest relevance first — the
// excess a storage budget archives.
func (r *EncounterRepo) LowestRelevanceActiveForSource(ctx context.Context, sourceID string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT DISTINCT e.artifact_id, a.relevance_score FROM encounters e
		 JOIN artifacts a ON a.id = e.artifact_id
		   AND a.deleted_at IS NULL AND a.lifecycle_tier = 'active' AND a.relevance_score IS NOT NULL
		 WHERE e.source_id = ?
		 ORDER BY a.relevance_score ASC, e.artifact_id LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("lowest-relevance artifacts for source %q: %w", sourceID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, fmt.Errorf("scan budget artifact: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MaxSourceWeightForArtifact returns the weight of the most authoritative source
// that has encountered the artifact (the relevance source_weight factor, §8.9).
// Defaults to 1.0 when the artifact has no encounters — a neutral weight that
// neither boosts nor suppresses.
func (r *EncounterRepo) MaxSourceWeightForArtifact(ctx context.Context, artifactID string) (float64, error) {
	var w float64
	if err := r.rq.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(s.source_weight), 1.0)
		 FROM encounters e JOIN sources s ON s.id = e.source_id
		 WHERE e.artifact_id = ?`, artifactID).Scan(&w); err != nil {
		return 0, fmt.Errorf("max source weight for %q: %w", artifactID, err)
	}
	return w, nil
}
