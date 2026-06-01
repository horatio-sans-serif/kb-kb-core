package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// Privacy: this intentionally does not filter by privacy_tier — search serves the
// single owner behind the bearer token, so all live tiers are searchable. A
// future lower-trust read path (see KB_LISTEN_ADDR) must add a tier ceiling here.
func (r *EnrichmentRepo) ForEachLatestEmbedding(ctx context.Context, fn func(artifactID, payload string) error) error {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT artifact_id, payload FROM (
		   SELECT e.artifact_id, e.payload,
		          ROW_NUMBER() OVER (PARTITION BY e.artifact_id ORDER BY e.created_at DESC, e.id DESC) AS rn
		   FROM enrichments e
		   JOIN artifacts a ON a.id = e.artifact_id AND a.deleted_at IS NULL
		   WHERE e.kind = 'embedding'
		 ) WHERE rn = 1`)
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
