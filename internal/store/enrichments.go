package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Enrichment is a versioned, schema-validated output of one pipeline stage for
// one artifact (design 6.4). Embedding vectors also travel here as payload data
// so every replica can rebuild its local ANN index (6.0).
type Enrichment struct {
	ID            string
	ArtifactID    string
	Kind          string // summary | tags | critical_analysis | relevance | embedding | ...
	Payload       string // JSON
	Model         string
	PromptVersion string
	CreatedAt     string
}

type EnrichmentRepo struct{ q Querier }

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

// Get returns the enrichment by id, or ErrNotFound.
func (r *EnrichmentRepo) Get(ctx context.Context, id string) (*Enrichment, error) {
	var e Enrichment
	err := r.q.QueryRowContext(ctx,
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
