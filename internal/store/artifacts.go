package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Artifact is a canonical knowledge item (design 6.2). Nullable columns are
// pointers so SQL NULL round-trips correctly; Title/BodyMarkdown are plain
// strings that read NULL as "" (see scanArtifact).
type Artifact struct {
	ID             string
	Type           string
	CanonicalURL   *string
	ContentHash    *string
	Title          string
	BodyMarkdown   string
	PrivacyTier    string
	LifecycleTier  string
	Status         string
	RelevanceScore *float64
	FirstSeenAt    string
	LastSeenAt     string
	LastEngagedAt  *string
	DeletedAt      *string
	CreatedAt      string
	UpdatedAt      string
}

type ArtifactRepo struct{ q Querier }

const artifactColNames = `id, type, canonical_url, content_hash, title, body_markdown,
	privacy_tier, lifecycle_tier, status, relevance_score,
	first_seen_at, last_seen_at, last_engaged_at, deleted_at, created_at, updated_at`

const artifactSelect = `SELECT ` + artifactColNames + ` FROM artifacts`

// UpsertByCanonical inserts the artifact, or — when a live row already has the
// same canonical URL — bumps its last_seen_at/updated_at and returns it. This is
// the one-artifact-many-encounters keystone (Principle 1), atomic in a single
// statement so concurrent callers cannot create a duplicate.
//
// A nil CanonicalURL never conflicts, so it always inserts. The partial unique
// index is scoped to live rows, so a tombstoned URL is free for a new artifact
// (a re-sighting after soft-delete is a new artifact, not a resurrection).
// Caller-supplied type/privacy/lifecycle/status/score are honored on insert;
// on conflict only last_seen_at/updated_at change (a re-sighting must not
// clobber an existing artifact's enriched fields).
func (r *ArtifactRepo) UpsertByCanonical(ctx context.Context, a *Artifact) (*Artifact, error) {
	if a.ID == "" {
		a.ID = NewID()
	}
	ts := now()
	if a.FirstSeenAt == "" {
		a.FirstSeenAt = ts
	}
	if a.LastSeenAt == "" {
		a.LastSeenAt = ts
	}
	if a.PrivacyTier == "" {
		a.PrivacyTier = "personal"
	}
	if a.LifecycleTier == "" {
		a.LifecycleTier = "active"
	}
	if a.Status == "" {
		a.Status = "ingested"
	}

	row := r.q.QueryRowContext(ctx,
		`INSERT INTO artifacts(id, type, canonical_url, content_hash, title, body_markdown,
			privacy_tier, lifecycle_tier, status, relevance_score,
			first_seen_at, last_seen_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(canonical_url) WHERE canonical_url IS NOT NULL AND deleted_at IS NULL
		 DO UPDATE SET last_seen_at = excluded.last_seen_at, updated_at = excluded.updated_at
		 RETURNING `+artifactColNames,
		a.ID, a.Type, a.CanonicalURL, a.ContentHash, a.Title, a.BodyMarkdown,
		a.PrivacyTier, a.LifecycleTier, a.Status, a.RelevanceScore,
		a.FirstSeenAt, a.LastSeenAt, ts, ts)

	got, err := scanArtifact(row)
	if err != nil {
		return nil, fmt.Errorf("upsert artifact: %w", err)
	}
	return got, nil
}

func (r *ArtifactRepo) Get(ctx context.Context, id string) (*Artifact, error) {
	a, err := scanArtifact(r.q.QueryRowContext(ctx, artifactSelect+` WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get artifact %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get artifact %q: %w", id, err)
	}
	return a, nil
}

// List returns live (non-tombstoned) artifacts, ordered by id (ULIDs are
// monotonic by creation time and replica-safe, unlike string timestamps).
func (r *ArtifactRepo) List(ctx context.Context) ([]*Artifact, error) {
	return r.list(ctx, ` WHERE deleted_at IS NULL ORDER BY id`)
}

// ListIncludingDeleted returns all artifacts, tombstoned or not, ordered by id.
func (r *ArtifactRepo) ListIncludingDeleted(ctx context.Context) ([]*Artifact, error) {
	return r.list(ctx, ` ORDER BY id`)
}

func (r *ArtifactRepo) list(ctx context.Context, clause string) ([]*Artifact, error) {
	rows, err := r.q.QueryContext(ctx, artifactSelect+clause)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()

	var out []*Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SoftDelete sets the tombstone (deleted_at). Idempotent: a no-op on an already
// deleted row. Tombstones, not hard deletes, so the row merges cleanly under
// cr-sqlite (design 6.0).
func (r *ArtifactRepo) SoftDelete(ctx context.Context, id string) error {
	ts := now()
	_, err := r.q.ExecContext(ctx,
		`UPDATE artifacts SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		ts, ts, id)
	if err != nil {
		return fmt.Errorf("soft delete artifact %q: %w", id, err)
	}
	return nil
}

func scanArtifact(s scanner) (*Artifact, error) {
	var a Artifact
	var title, body sql.NullString // nullable in schema; read NULL as ""
	if err := s.Scan(
		&a.ID, &a.Type, &a.CanonicalURL, &a.ContentHash, &title, &body,
		&a.PrivacyTier, &a.LifecycleTier, &a.Status, &a.RelevanceScore,
		&a.FirstSeenAt, &a.LastSeenAt, &a.LastEngagedAt, &a.DeletedAt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scan artifact: %w", err)
	}
	a.Title = title.String
	a.BodyMarkdown = body.String
	return &a, nil
}
