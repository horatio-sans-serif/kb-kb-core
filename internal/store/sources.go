package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Source is defined in internal/kb (aliased in types.go).
type SourceRepo struct{ q, rq Querier } // q: write pool; rq: read pool

// Create inserts a source, assigning a ULID if none is set and defaulting the
// privacy tier to "personal". source_weight and enabled take their schema
// defaults.
func (r *SourceRepo) Create(ctx context.Context, s *Source) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	if s.Config == "" {
		s.Config = "{}"
	}
	if s.DefaultPrivacy == "" {
		s.DefaultPrivacy = "personal"
	}
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO sources(id, kind, connector, config, default_privacy, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		s.ID, s.Kind, s.Connector, s.Config, s.DefaultPrivacy, now())
	if err != nil {
		return fmt.Errorf("create source %q: %w", s.ID, err)
	}
	return nil
}

func (r *SourceRepo) Get(ctx context.Context, id string) (*Source, error) {
	var s Source
	var enabled int
	err := r.rq.QueryRowContext(ctx,
		`SELECT id, kind, connector, config, default_privacy, source_weight, enabled, created_at
		 FROM sources WHERE id = ?`, id).
		Scan(&s.ID, &s.Kind, &s.Connector, &s.Config, &s.DefaultPrivacy, &s.SourceWeight, &enabled, &s.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get source %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get source %q: %w", id, err)
	}
	s.Enabled = enabled != 0
	return &s, nil
}
