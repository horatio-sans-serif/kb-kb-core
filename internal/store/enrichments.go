package store

import (
	"context"
	"database/sql"
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

type EnrichmentRepo struct{ db *sql.DB }

func (r *EnrichmentRepo) Create(ctx context.Context, e *Enrichment) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO enrichments(id, artifact_id, kind, payload, model, prompt_version)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.ArtifactID, e.Kind, e.Payload, e.Model, e.PromptVersion)
	if err != nil {
		return fmt.Errorf("create enrichment: %w", err)
	}
	return nil
}

func (r *EnrichmentRepo) Get(ctx context.Context, id string) (*Enrichment, error) {
	var e Enrichment
	err := r.db.QueryRowContext(ctx,
		`SELECT id, artifact_id, kind, payload, model, prompt_version, created_at
		 FROM enrichments WHERE id = ?`, id).
		Scan(&e.ID, &e.ArtifactID, &e.Kind, &e.Payload, &e.Model, &e.PromptVersion, &e.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get enrichment %q: %w", id, err)
	}
	return &e, nil
}
