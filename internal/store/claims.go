package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Claim and Conflict are defined in internal/kb (aliased in types.go).
type ClaimRepo struct{ q, rq Querier }    // q: write pool; rq: read pool
type ConflictRepo struct{ q, rq Querier } // q: write pool; rq: read pool

const claimCols = `id, artifact_id, statement, embedding, created_at`

// Create inserts a claim, assigning a ULID and CreatedAt. The embedding is stored
// as a JSON float array, or NULL when absent.
func (r *ClaimRepo) Create(ctx context.Context, c *Claim) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.CreatedAt == "" {
		c.CreatedAt = now()
	}
	var emb any
	if len(c.Embedding) > 0 {
		b, err := json.Marshal(c.Embedding)
		if err != nil {
			return fmt.Errorf("marshal claim embedding: %w", err)
		}
		emb = string(b)
	}
	if _, err := r.q.ExecContext(ctx,
		`INSERT INTO claims(`+claimCols+`) VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.ArtifactID, c.Statement, emb, c.CreatedAt); err != nil {
		return fmt.Errorf("create claim for %q: %w", c.ArtifactID, err)
	}
	return nil
}

// ListByArtifact returns an artifact's claims, oldest first.
func (r *ClaimRepo) ListByArtifact(ctx context.Context, artifactID string) ([]*Claim, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT `+claimCols+` FROM claims WHERE artifact_id = ? ORDER BY created_at, id`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list claims for %q: %w", artifactID, err)
	}
	defer rows.Close()
	return scanClaims(rows)
}

// ListExcludingArtifact returns the embedded claims of every OTHER live artifact —
// the contradiction-comparison candidate set for a newly-claimed artifact. Claims
// without an embedding (nothing to vector-prefilter on) and claims of tombstoned
// artifacts are excluded. When cloudSafeOnly is set (a remote LLM judges the
// comparison), claims of sensitive/restricted artifacts are also excluded so they
// never leave the machine (§8.17).
func (r *ClaimRepo) ListExcludingArtifact(ctx context.Context, artifactID string, cloudSafeOnly bool) ([]*Claim, error) {
	q := `SELECT c.id, c.artifact_id, c.statement, c.embedding, c.created_at FROM claims c
	      JOIN artifacts a ON a.id = c.artifact_id AND a.deleted_at IS NULL
	      WHERE c.artifact_id != ? AND c.embedding IS NOT NULL`
	if cloudSafeOnly {
		q += ` AND a.privacy_tier IN ('public','personal')`
	}
	q += ` ORDER BY c.created_at, c.id`
	rows, err := r.rq.QueryContext(ctx, q, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list claims excluding %q: %w", artifactID, err)
	}
	defer rows.Close()
	return scanClaims(rows)
}

func scanClaims(rows *sql.Rows) ([]*Claim, error) {
	var out []*Claim
	for rows.Next() {
		var c Claim
		var emb sql.NullString
		if err := rows.Scan(&c.ID, &c.ArtifactID, &c.Statement, &emb, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		if emb.Valid && emb.String != "" {
			if err := json.Unmarshal([]byte(emb.String), &c.Embedding); err != nil {
				return nil, fmt.Errorf("unmarshal claim %q embedding: %w", c.ID, err)
			}
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

const conflictCols = `id, claim_a, claim_b, status, rationale, created_at`

// Create inserts a conflict between two claims, canonicalizing the pair (claim_a <
// claim_b) so the same contradiction discovered from either side dedups against
// the unique index (INSERT OR IGNORE). Idempotent.
func (r *ConflictRepo) Create(ctx context.Context, c *Conflict) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.CreatedAt == "" {
		c.CreatedAt = now()
	}
	if c.Status == "" {
		c.Status = "open"
	}
	a, b := c.ClaimA, c.ClaimB
	if a > b {
		a, b = b, a
	}
	if _, err := r.q.ExecContext(ctx,
		`INSERT OR IGNORE INTO conflicts(`+conflictCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, a, b, c.Status, nullIfEmpty(c.Rationale), c.CreatedAt); err != nil {
		return fmt.Errorf("create conflict %q/%q: %w", a, b, err)
	}
	return nil
}

// ListByStatus returns conflicts in the given status (open|resolved|dismissed),
// newest first.
func (r *ConflictRepo) ListByStatus(ctx context.Context, status string, limit int) ([]*Conflict, error) {
	if limit <= 0 {
		limit = surfaceDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT `+conflictCols+` FROM conflicts WHERE status = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		status, limit)
	if err != nil {
		return nil, fmt.Errorf("list %s conflicts: %w", status, err)
	}
	defer rows.Close()

	var out []*Conflict
	for rows.Next() {
		c, err := scanConflict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetStatus moves a conflict to a new status with an optional rationale. Returns
// ErrNotFound if no conflict matches.
func (r *ConflictRepo) SetStatus(ctx context.Context, id, status, rationale string) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE conflicts SET status = ?, rationale = ? WHERE id = ?`,
		status, nullIfEmpty(rationale), id)
	if err != nil {
		return fmt.Errorf("set conflict %q status: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set conflict %q status (rows): %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("set conflict %q status: %w", id, ErrNotFound)
	}
	return nil
}

func scanConflict(sc scanner) (*Conflict, error) {
	var c Conflict
	var rationale sql.NullString
	if err := sc.Scan(&c.ID, &c.ClaimA, &c.ClaimB, &c.Status, &rationale, &c.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan conflict: %w", err)
	}
	c.Rationale = rationale.String
	return &c, nil
}
