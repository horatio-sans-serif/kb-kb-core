package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// RenderRecord is defined in internal/kb (aliased in types.go).
type RenderRepo struct{ q, rq Querier } // q: write pool; rq: read pool

const renderCols = `id, format, source_query, artifact_ids, output_path, privacy_max, created_at`

// Create inserts a render-provenance row, assigning a ULID and CreatedAt if
// unset. artifact_ids is stored as a JSON array; an empty list is stored as "[]".
func (r *RenderRepo) Create(ctx context.Context, rec *RenderRecord) error {
	if rec.ID == "" {
		rec.ID = NewID()
	}
	if rec.CreatedAt == "" {
		rec.CreatedAt = now()
	}
	ids, err := json.Marshal(rec.ArtifactIDs)
	if err != nil {
		return fmt.Errorf("marshal render artifact ids: %w", err)
	}
	if rec.ArtifactIDs == nil {
		ids = []byte("[]") // nil marshals to "null"; store an explicit empty array
	}
	if _, err := r.q.ExecContext(ctx,
		`INSERT INTO renders(`+renderCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Format, nullIfEmpty(rec.SourceQuery), string(ids),
		nullIfEmpty(rec.OutputPath), rec.PrivacyMax, rec.CreatedAt); err != nil {
		return fmt.Errorf("create render %q: %w", rec.ID, err)
	}
	return nil
}

// Get returns the render by id, or ErrNotFound.
func (r *RenderRepo) Get(ctx context.Context, id string) (*RenderRecord, error) {
	rec, err := scanRender(r.rq.QueryRowContext(ctx, `SELECT `+renderCols+` FROM renders WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get render %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get render %q: %w", id, err)
	}
	return rec, nil
}

// List returns all renders, newest first (the render history a UI shows).
func (r *RenderRepo) List(ctx context.Context) ([]*RenderRecord, error) {
	rows, err := r.rq.QueryContext(ctx, `SELECT `+renderCols+` FROM renders ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list renders: %w", err)
	}
	defer rows.Close()

	var out []*RenderRecord
	for rows.Next() {
		rec, err := scanRender(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanRender(sc scanner) (*RenderRecord, error) {
	var rec RenderRecord
	var sourceQuery, ids, outputPath sql.NullString
	if err := sc.Scan(
		&rec.ID, &rec.Format, &sourceQuery, &ids, &outputPath, &rec.PrivacyMax, &rec.CreatedAt,
	); err != nil {
		return nil, err
	}
	rec.SourceQuery = sourceQuery.String
	rec.OutputPath = outputPath.String
	if ids.Valid && ids.String != "" {
		if err := json.Unmarshal([]byte(ids.String), &rec.ArtifactIDs); err != nil {
			return nil, fmt.Errorf("unmarshal render %q artifact ids: %w", rec.ID, err)
		}
	}
	return &rec, nil
}
