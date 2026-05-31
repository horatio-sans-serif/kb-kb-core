package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Source is a registered origin of artifacts (design 6.1).
type Source struct {
	ID             string
	Kind           string // PULL | PUSH | AMBIENT
	Connector      string
	Config         string // JSON
	DefaultPrivacy string
	SourceWeight   float64
	Enabled        bool
	CreatedAt      string
}

type SourceRepo struct{ q Querier }

// Create inserts a source, assigning a ULID if none is set. Optional columns
// (default_privacy, source_weight, enabled) take their schema defaults.
func (r *SourceRepo) Create(ctx context.Context, s *Source) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	if s.Config == "" {
		s.Config = "{}"
	}
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO sources(id, kind, connector, config, created_at) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.Kind, s.Connector, s.Config, now())
	if err != nil {
		return fmt.Errorf("create source %q: %w", s.ID, err)
	}
	return nil
}

func (r *SourceRepo) Get(ctx context.Context, id string) (*Source, error) {
	var s Source
	var enabled int
	err := r.q.QueryRowContext(ctx,
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
