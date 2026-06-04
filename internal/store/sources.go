package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Source is defined in internal/kb (aliased in types.go).
type SourceRepo struct{ q, rq Querier } // q: write pool; rq: read pool

const sourceCols = `id, kind, connector, config, schedule, auth_state,
	default_privacy, storage_budget, source_weight, enabled, created_at,
	cursor, last_fetched_at, health`

// Create inserts a source, assigning a ULID if none is set and applying the
// schema defaults (config "{}", privacy "personal", source_weight 1.0, enabled)
// into the struct as well, so the caller sees the persisted state without a
// re-read.
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
	if s.SourceWeight == 0 {
		s.SourceWeight = 1.0
	}
	s.Enabled = true
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO sources(id, kind, connector, config, schedule, auth_state,
			default_privacy, storage_budget, source_weight, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		s.ID, s.Kind, s.Connector, s.Config, s.Schedule, s.AuthState,
		s.DefaultPrivacy, s.StorageBudget, s.SourceWeight, now())
	if err != nil {
		return fmt.Errorf("create source %q: %w", s.ID, err)
	}
	return nil
}

// Get returns the source by id, or ErrNotFound.
func (r *SourceRepo) Get(ctx context.Context, id string) (*Source, error) {
	s, err := scanSource(r.rq.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM sources WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get source %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get source %q: %w", id, err)
	}
	return s, nil
}

// List returns all sources, ordered by creation. The scheduler (§8.1) and health
// monitor (§8.15) enumerate them.
func (r *SourceRepo) List(ctx context.Context) ([]*Source, error) {
	rows, err := r.rq.QueryContext(ctx, `SELECT `+sourceCols+` FROM sources ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Update writes the mutable columns of s (config, schedule, auth_state,
// default_privacy, storage_budget, source_weight, enabled). id and connector are
// immutable.
func (r *SourceRepo) Update(ctx context.Context, s *Source) error {
	_, err := r.q.ExecContext(ctx,
		`UPDATE sources SET config = ?, schedule = ?, auth_state = ?, default_privacy = ?,
			storage_budget = ?, source_weight = ?, enabled = ?
		 WHERE id = ?`,
		s.Config, s.Schedule, s.AuthState, s.DefaultPrivacy, s.StorageBudget,
		s.SourceWeight, btoi(s.Enabled), s.ID)
	if err != nil {
		return fmt.Errorf("update source %q: %w", s.ID, err)
	}
	return nil
}

// SetEnabled enables or disables a source. Disabling is the soft-delete for a
// source: the row (and its encounters' FK) is preserved, but the scheduler skips
// it. Sources are never hard-deleted (that would orphan encounters).
func (r *SourceRepo) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if _, err := r.q.ExecContext(ctx, `UPDATE sources SET enabled = ? WHERE id = ?`, btoi(enabled), id); err != nil {
		return fmt.Errorf("set source %q enabled=%v: %w", id, enabled, err)
	}
	return nil
}

// ListEnabledByKind returns enabled sources of one kind (e.g. "PULL"), ordered by
// creation — the scheduler's work list.
func (r *SourceRepo) ListEnabledByKind(ctx context.Context, kind string) ([]*Source, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT `+sourceCols+` FROM sources WHERE enabled = 1 AND kind = ? ORDER BY created_at, id`, kind)
	if err != nil {
		return nil, fmt.Errorf("list enabled %s sources: %w", kind, err)
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListWithBudget returns sources that have a storage_budget set — the work list
// for budget enforcement.
func (r *SourceRepo) ListWithBudget(ctx context.Context) ([]*Source, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT `+sourceCols+` FROM sources WHERE storage_budget IS NOT NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list budgeted sources: %w", err)
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateFetchState records the outcome of a poll: the new cursor, the time it ran,
// and the health snapshot. Narrow update of the fetch-state columns only.
func (r *SourceRepo) UpdateFetchState(ctx context.Context, id string, cursor, lastFetchedAt, health *string) error {
	if _, err := r.q.ExecContext(ctx,
		`UPDATE sources SET cursor = ?, last_fetched_at = ?, health = ? WHERE id = ?`,
		cursor, lastFetchedAt, health, id); err != nil {
		return fmt.Errorf("update fetch state for source %q: %w", id, err)
	}
	return nil
}

func scanSource(sc scanner) (*Source, error) {
	var s Source
	var enabled int
	if err := sc.Scan(
		&s.ID, &s.Kind, &s.Connector, &s.Config, &s.Schedule, &s.AuthState,
		&s.DefaultPrivacy, &s.StorageBudget, &s.SourceWeight, &enabled, &s.CreatedAt,
		&s.Cursor, &s.LastFetchedAt, &s.Health,
	); err != nil {
		return nil, err
	}
	s.Enabled = enabled != 0
	return &s, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
