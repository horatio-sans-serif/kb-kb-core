package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Artifact is a canonical knowledge item (design 6.2). Nullable columns are
// pointers so SQL NULL round-trips correctly.
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

type ArtifactRepo struct{ db *sql.DB }

const artifactCols = `SELECT id, type, canonical_url, content_hash, title, body_markdown,
	privacy_tier, lifecycle_tier, status, relevance_score,
	first_seen_at, last_seen_at, last_engaged_at, deleted_at, created_at, updated_at
	FROM artifacts`

// UpsertByCanonical returns the existing live artifact with the same canonical
// URL (bumping last_seen_at / updated_at), or inserts a new one. When
// CanonicalURL is nil it always inserts — content-hash dedup is Phase 1 (§8.2).
// This is the one-artifact-many-encounters keystone in code (Principle 1).
func (r *ArtifactRepo) UpsertByCanonical(ctx context.Context, a *Artifact) (*Artifact, error) {
	if a.CanonicalURL != nil {
		var id string
		err := r.db.QueryRowContext(ctx,
			`SELECT id FROM artifacts WHERE canonical_url = ? AND deleted_at IS NULL`,
			*a.CanonicalURL).Scan(&id)
		switch {
		case err == nil:
			ts := now()
			if _, err := r.db.ExecContext(ctx,
				`UPDATE artifacts SET last_seen_at = ?, updated_at = ? WHERE id = ?`,
				ts, ts, id); err != nil {
				return nil, fmt.Errorf("touch artifact %q: %w", id, err)
			}
			return r.Get(ctx, id)
		case err == sql.ErrNoRows:
			// fall through to insert
		default:
			return nil, fmt.Errorf("lookup canonical %q: %w", *a.CanonicalURL, err)
		}
	}
	return r.insert(ctx, a)
}

func (r *ArtifactRepo) insert(ctx context.Context, a *Artifact) (*Artifact, error) {
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
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO artifacts(id, type, canonical_url, content_hash, title, body_markdown,
			first_seen_at, last_seen_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Type, a.CanonicalURL, a.ContentHash, a.Title, a.BodyMarkdown,
		a.FirstSeenAt, a.LastSeenAt, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert artifact: %w", err)
	}
	return r.Get(ctx, a.ID)
}

func (r *ArtifactRepo) Get(ctx context.Context, id string) (*Artifact, error) {
	return scanArtifact(r.db.QueryRowContext(ctx, artifactCols+` WHERE id = ?`, id))
}

// List returns live (non-tombstoned) artifacts.
func (r *ArtifactRepo) List(ctx context.Context) ([]*Artifact, error) {
	return r.list(ctx, ` WHERE deleted_at IS NULL ORDER BY created_at`)
}

// ListIncludingDeleted returns all artifacts, tombstoned or not.
func (r *ArtifactRepo) ListIncludingDeleted(ctx context.Context) ([]*Artifact, error) {
	return r.list(ctx, ` ORDER BY created_at`)
}

func (r *ArtifactRepo) list(ctx context.Context, where string) ([]*Artifact, error) {
	rows, err := r.db.QueryContext(ctx, artifactCols+where)
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
	_, err := r.db.ExecContext(ctx,
		`UPDATE artifacts SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		ts, ts, id)
	if err != nil {
		return fmt.Errorf("soft delete artifact %q: %w", id, err)
	}
	return nil
}

func scanArtifact(s scanner) (*Artifact, error) {
	var a Artifact
	if err := s.Scan(
		&a.ID, &a.Type, &a.CanonicalURL, &a.ContentHash, &a.Title, &a.BodyMarkdown,
		&a.PrivacyTier, &a.LifecycleTier, &a.Status, &a.RelevanceScore,
		&a.FirstSeenAt, &a.LastSeenAt, &a.LastEngagedAt, &a.DeletedAt, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scan artifact: %w", err)
	}
	return &a, nil
}
