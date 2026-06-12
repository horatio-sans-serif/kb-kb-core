package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"kb/internal/kb"
)

// Enrichment is defined in internal/kb (aliased in types.go).
type EnrichmentRepo struct{ q, rq Querier } // q: write pool; rq: read pool

// Create inserts an enrichment row, assigning a ULID if none is set.
func (r *EnrichmentRepo) Create(ctx context.Context, e *Enrichment) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO enrichments(id, artifact_id, kind, payload, model, prompt_version, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ArtifactID, e.Kind, e.Payload, e.Model, e.PromptVersion, now())
	if err != nil {
		return fmt.Errorf("create enrichment: %w", err)
	}
	return nil
}

// ListForArtifact returns all enrichments for an artifact, oldest first.
func (r *EnrichmentRepo) ListForArtifact(ctx context.Context, artifactID string) ([]*Enrichment, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT id, artifact_id, kind, payload, model, prompt_version, created_at
		 FROM enrichments WHERE artifact_id = ? ORDER BY created_at, id`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list enrichments for %q: %w", artifactID, err)
	}
	defer rows.Close()

	var out []*Enrichment
	for rows.Next() {
		var e Enrichment
		if err := rows.Scan(&e.ID, &e.ArtifactID, &e.Kind, &e.Payload, &e.Model, &e.PromptVersion, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan enrichment: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// LatestByKind returns the most recent enrichment of a kind for an artifact, or
// ErrNotFound. The relevance stage uses it to read back the embedding vector the
// embed stage wrote (the stages communicate only through stored rows).
func (r *EnrichmentRepo) LatestByKind(ctx context.Context, artifactID, kind string) (*Enrichment, error) {
	var e Enrichment
	err := r.rq.QueryRowContext(ctx,
		`SELECT id, artifact_id, kind, payload, model, prompt_version, created_at
		 FROM enrichments WHERE artifact_id = ? AND kind = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, artifactID, kind).
		Scan(&e.ID, &e.ArtifactID, &e.Kind, &e.Payload, &e.Model, &e.PromptVersion, &e.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("latest %q enrichment for %q: %w", kind, artifactID, ErrNotFound)
		}
		return nil, fmt.Errorf("latest %q enrichment for %q: %w", kind, artifactID, err)
	}
	return &e, nil
}

// ForEachLatestEmbedding streams the latest `embedding` enrichment payload for
// every live (non-tombstoned) artifact — the candidate set for semantic search —
// invoking fn once per row with the raw JSON payload (the search layer parses it
// into kb.EmbeddingPayload). A window function picks the most recent row per
// artifact in one pass; streaming lets the caller keep only a bounded top-k
// rather than materializing every vector. Returning an error from fn aborts the
// scan. The vec0 ANN index (deferred) replaces the full scan for sublinear
// retrieval at scale.
//
// Privacy: this is the owner-ceiling (unfiltered) entry point. The lower-trust read
// path (§8.17) goes through ForEachLatestEmbeddingWithin with its configured ceiling;
// this wrapper passes kb.Restricted, which streams every tier.
func (r *EnrichmentRepo) ForEachLatestEmbedding(ctx context.Context, fn func(artifactID, payload string) error) error {
	// Unfiltered (owner ceiling): the vec0 index builds over every embedding; the read
	// ceiling is applied at query time, not at index-build time.
	return r.ForEachLatestEmbeddingWithin(ctx, kb.Restricted, fn)
}

// ForEachLatestEmbeddingWithin is ForEachLatestEmbedding scoped to a read ceiling:
// only artifacts at or below ceiling are streamed (§8.17), so the brute-force
// semantic search a lower-trust reader runs never scores content it may not see.
func (r *EnrichmentRepo) ForEachLatestEmbeddingWithin(ctx context.Context, ceiling kb.PrivacyTier, fn func(artifactID, payload string) error) error {
	tier, args := tierClause("a.privacy_tier", ceiling)
	rows, err := r.rq.QueryContext(ctx,
		`SELECT artifact_id, payload FROM (
		   SELECT e.artifact_id, e.payload,
		          ROW_NUMBER() OVER (PARTITION BY e.artifact_id ORDER BY e.created_at DESC, e.id DESC) AS rn
		   FROM enrichments e
		   JOIN artifacts a ON a.id = e.artifact_id AND a.deleted_at IS NULL AND `+tier+`
		   WHERE e.kind = 'embedding'
		 ) WHERE rn = 1`, args...)
	if err != nil {
		return fmt.Errorf("latest embeddings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var artifactID, payload string
		if err := rows.Scan(&artifactID, &payload); err != nil {
			return fmt.Errorf("scan embedding row: %w", err)
		}
		if err := fn(artifactID, payload); err != nil {
			return err
		}
	}
	return rows.Err()
}

// EmbeddingRow is one artifact's embedding payload (raw JSON — the caller parses
// it into kb.EmbeddingPayload).
type EmbeddingRow struct {
	ArtifactID string
	Payload    string
}

// EngagedEmbeddings returns the latest embedding payload of each live artifact
// the user has engaged with (last_engaged_at set), most-recently-engaged first,
// capped at limit and excluding excludeID. These feed the living profile (§8.9):
// recently-engaged content shapes what scores as relevant. excludeID keeps the
// artifact currently being scored out of its own profile (no self-match).
//
// Privacy: this does not filter by privacy_tier, and the rule is sharper here than
// for ForEachLatestEmbedding. A sensitive/restricted engaged artifact's embedding
// influences the stored relevance_score of OTHER (possibly public) artifacts. The
// read-ceiling read path (§8.17) does NOT close this channel — it filters reads by
// tier, but the cross-tier influence is baked into relevance_score at scoring time,
// upstream of any read filter. This is a known §8.17 limitation, acceptable now
// because relevance_score is never serialized out to a read token (the read surface
// exposes ranked artifacts, not the score itself). Closing it would require filtering
// engaged rows by a tier ceiling, or segregating scores per ceiling.
func (r *EnrichmentRepo) EngagedEmbeddings(ctx context.Context, excludeID string, limit int) ([]EmbeddingRow, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT artifact_id, payload FROM (
		   SELECT e.artifact_id, e.payload, a.last_engaged_at,
		          ROW_NUMBER() OVER (PARTITION BY e.artifact_id ORDER BY e.created_at DESC, e.id DESC) AS rn
		   FROM enrichments e
		   JOIN artifacts a ON a.id = e.artifact_id
		     AND a.deleted_at IS NULL AND a.last_engaged_at IS NOT NULL
		   WHERE e.kind = 'embedding' AND e.artifact_id <> ?
		 ) WHERE rn = 1
		 ORDER BY last_engaged_at DESC
		 LIMIT ?`, excludeID, limit)
	if err != nil {
		return nil, fmt.Errorf("engaged embeddings: %w", err)
	}
	defer rows.Close()

	var out []EmbeddingRow
	for rows.Next() {
		var er EmbeddingRow
		if err := rows.Scan(&er.ArtifactID, &er.Payload); err != nil {
			return nil, fmt.Errorf("scan engaged embedding: %w", err)
		}
		out = append(out, er)
	}
	return out, rows.Err()
}

// CountByKind returns the number of enrichment rows of a kind — a cheap change
// detector for the vec0 index. A new or re-run embedding adds a row and a
// hard-delete removes one, so the count moves for those. It does NOT move for a
// merge that re-points a loser's embedding to the survivor (an UPDATE, not an
// insert/delete), so the survivor can hold a slightly stale vector until the next
// count-changing event; the search live-join already drops the tombstoned loser, so
// this is a minor, self-healing relevance imperfection, not a correctness break.
func (r *EnrichmentRepo) CountByKind(ctx context.Context, kind string) (int, error) {
	var n int
	if err := r.rq.QueryRowContext(ctx, `SELECT COUNT(*) FROM enrichments WHERE kind = ?`, kind).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %q enrichments: %w", kind, err)
	}
	return n, nil
}

// Get returns the enrichment by id, or ErrNotFound.
func (r *EnrichmentRepo) Get(ctx context.Context, id string) (*Enrichment, error) {
	var e Enrichment
	err := r.rq.QueryRowContext(ctx,
		`SELECT id, artifact_id, kind, payload, model, prompt_version, created_at
		 FROM enrichments WHERE id = ?`, id).
		Scan(&e.ID, &e.ArtifactID, &e.Kind, &e.Payload, &e.Model, &e.PromptVersion, &e.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get enrichment %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get enrichment %q: %w", id, err)
	}
	return &e, nil
}
