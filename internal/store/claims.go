package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"kb/internal/kb"
)

// Claim and Conflict are defined in internal/kb (aliased in types.go).
type ClaimRepo struct{ q, rq Querier }    // q: write pool; rq: read pool
type ConflictRepo struct{ q, rq Querier } // q: write pool; rq: read pool

const claimCols = `id, artifact_id, statement, embedding, created_at`

// Create inserts a claim, assigning a ULID and CreatedAt. The embedding is stored
// as a JSON float array, or NULL when absent. Idempotent on (artifact_id,
// statement): re-extracting the same statement is a no-op, keeping the original
// row's id stable. Dedup is application-level (find-then-insert, atomic via
// withTx), not migration 0009's unique index, which cr-sqlite (§8.16) forbids on a
// replicated table — a cross-replica duplicate is reconciled by ReconcileDuplicates.
// To replace an artifact's claim set wholesale (a future re-enrich), use
// DeleteByArtifact first — removed statements are not reclaimed by a re-extract.
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
	return withTx(ctx, r.q, func(q Querier) error {
		var exists bool
		if err := q.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM claims WHERE artifact_id = ? AND statement = ?)`,
			c.ArtifactID, c.Statement).Scan(&exists); err != nil {
			return fmt.Errorf("find claim for %q: %w", c.ArtifactID, err)
		}
		if exists {
			return nil
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO claims(`+claimCols+`) VALUES (?, ?, ?, ?, ?)`,
			c.ID, c.ArtifactID, c.Statement, emb, c.CreatedAt); err != nil {
			return fmt.Errorf("create claim for %q: %w", c.ArtifactID, err)
		}
		return nil
	})
}

// ReconcileDuplicates merges cross-replica duplicate claims — rows sharing
// (artifact_id, statement) — into the lowest-id survivor (deterministic across
// replicas): it re-points the loser's conflicts (claim_a/claim_b) to the survivor,
// drops the degenerate self-conflicts that re-point can create, and deletes the
// loser claim. Returns how many losers it merged. Each merge runs in its own
// transaction; a per-group failure is collected and the rest proceed.
func (r *ClaimRepo) ReconcileDuplicates(ctx context.Context) (int, error) {
	groups, err := r.duplicateGroups(ctx)
	if err != nil {
		return 0, err
	}
	merged := 0
	var errs []error
	for _, g := range groups {
		survivor := g[0] // lowest id (groups come id-ascending)
		for _, loser := range g[1:] {
			err := withTx(ctx, r.q, func(q Querier) error {
				return (&ClaimRepo{q: q, rq: q}).mergeInto(ctx, survivor, loser)
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("merge claim %q into %q: %w", loser, survivor, err))
				if ctx.Err() != nil {
					return merged, errors.Join(errs...)
				}
				continue
			}
			merged++
		}
	}
	return merged, errors.Join(errs...)
}

// duplicateGroups returns the id groups (2+, id-ascending) of claims sharing an
// (artifact_id, statement). Boundaries are detected on the key columns directly (no
// concatenation), so a statement containing any byte cannot merge unrelated groups.
func (r *ClaimRepo) duplicateGroups(ctx context.Context) ([][]string, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT artifact_id, statement, id FROM claims
		 WHERE (artifact_id, statement) IN (
		   SELECT artifact_id, statement FROM claims
		   GROUP BY artifact_id, statement HAVING COUNT(*) > 1)
		 ORDER BY artifact_id, statement, id`)
	if err != nil {
		return nil, fmt.Errorf("duplicate claim groups: %w", err)
	}
	defer rows.Close()

	var out [][]string
	var curA, curS string
	var cur []string
	first := true
	for rows.Next() {
		var a, s, id string
		if err := rows.Scan(&a, &s, &id); err != nil {
			return nil, fmt.Errorf("scan claim dup row: %w", err)
		}
		if first || a != curA || s != curS {
			if len(cur) > 1 {
				out = append(out, cur)
			}
			curA, curS, cur, first = a, s, nil, false
		}
		cur = append(cur, id)
	}
	if len(cur) > 1 {
		out = append(out, cur)
	}
	return out, rows.Err()
}

// mergeInto folds the loser claim into the survivor; r must be transaction-bound.
func (r *ClaimRepo) mergeInto(ctx context.Context, survivor, loser string) error {
	for _, col := range []string{"claim_a", "claim_b"} {
		if _, err := r.q.ExecContext(ctx,
			`UPDATE OR IGNORE conflicts SET `+col+` = ? WHERE `+col+` = ?`, survivor, loser); err != nil {
			return fmt.Errorf("re-point conflicts.%s: %w", col, err)
		}
		if _, err := r.q.ExecContext(ctx, `DELETE FROM conflicts WHERE `+col+` = ?`, loser); err != nil {
			return fmt.Errorf("drop leftover conflicts.%s: %w", col, err)
		}
	}
	// A loser-vs-survivor conflict becomes survivor-vs-survivor after the re-point.
	if _, err := r.q.ExecContext(ctx, `DELETE FROM conflicts WHERE claim_a = claim_b`); err != nil {
		return fmt.Errorf("drop self-conflicts: %w", err)
	}
	if _, err := r.q.ExecContext(ctx, `DELETE FROM claims WHERE id = ?`, loser); err != nil {
		return fmt.Errorf("delete loser claim %q: %w", loser, err)
	}
	return nil
}

// DeleteByArtifact removes an artifact's claims and the conflict rows that
// reference them (either side) — the cascade a re-enrich or lifecycle GC needs.
// Synced tables no longer carry foreign keys (§8.16), so this cascade is now
// application-enforced: it deletes conflicts first so no conflict is left dangling
// against a deleted claim (which ListByStatus's JOINs would silently drop).
func (r *ClaimRepo) DeleteByArtifact(ctx context.Context, artifactID string) error {
	if _, err := r.q.ExecContext(ctx,
		`DELETE FROM conflicts WHERE claim_a IN (SELECT id FROM claims WHERE artifact_id = ?)
		    OR claim_b IN (SELECT id FROM claims WHERE artifact_id = ?)`,
		artifactID, artifactID); err != nil {
		return fmt.Errorf("delete conflicts for artifact %q: %w", artifactID, err)
	}
	if _, err := r.q.ExecContext(ctx, `DELETE FROM claims WHERE artifact_id = ?`, artifactID); err != nil {
		return fmt.Errorf("delete claims for artifact %q: %w", artifactID, err)
	}
	return nil
}

// Get returns a claim by id, or ErrNotFound.
func (r *ClaimRepo) Get(ctx context.Context, id string) (*Claim, error) {
	rows, err := r.rq.QueryContext(ctx, `SELECT `+claimCols+` FROM claims WHERE id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("get claim %q: %w", id, err)
	}
	defer rows.Close()
	cs, err := scanClaims(rows)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, fmt.Errorf("get claim %q: %w", id, ErrNotFound)
	}
	return cs[0], nil
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
		// Mirror kb.PrivacyTier.AllowsCloud() — keep these two in sync if the tier
		// set ever changes.
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
// claim_b) so the same contradiction discovered from either side dedups. Idempotent
// on (claim_a, claim_b). Dedup is application-level (find-then-insert, atomic via
// withTx), not migration 0008's unique index, which cr-sqlite (§8.16) forbids on a
// replicated table — a cross-replica duplicate is reconciled by DedupDuplicates.
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
	return withTx(ctx, r.q, func(q Querier) error {
		var exists bool
		if err := q.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM conflicts WHERE claim_a = ? AND claim_b = ?)`, a, b).Scan(&exists); err != nil {
			return fmt.Errorf("find conflict %q/%q: %w", a, b, err)
		}
		if exists {
			return nil
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO conflicts(`+conflictCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
			c.ID, a, b, c.Status, nullIfEmpty(c.Rationale), c.CreatedAt); err != nil {
			return fmt.Errorf("create conflict %q/%q: %w", a, b, err)
		}
		return nil
	})
}

// DedupDuplicates collapses cross-replica duplicate conflicts — rows sharing
// (claim_a, claim_b) — keeping the lowest id (deterministic across replicas).
// Conflicts carry no references, so the extras are dropped. Returns how many it
// removed.
func (r *ConflictRepo) DedupDuplicates(ctx context.Context) (int, error) {
	var n int64
	err := withTx(ctx, r.q, func(q Querier) error {
		res, err := q.ExecContext(ctx,
			`DELETE FROM conflicts WHERE id NOT IN (SELECT MIN(id) FROM conflicts GROUP BY claim_a, claim_b)`)
		if err != nil {
			return fmt.Errorf("dedup conflicts: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

// ListByStatus returns conflicts in the given status (open|resolved|dismissed),
// newest first. Conflicts whose either claim belongs to a tombstoned artifact are
// excluded — a soft-deleted artifact's contradictions are no longer actionable.
func (r *ConflictRepo) ListByStatus(ctx context.Context, status string, limit int, ceiling kb.PrivacyTier) ([]*Conflict, error) {
	if limit <= 0 {
		limit = surfaceDefaultLimit
	}
	// A conflict is visible only when BOTH contradicting claims sit on in-ceiling
	// artifacts; otherwise it would reveal the existence (and gist) of an
	// above-ceiling artifact to a lower-trust reader.
	tierA, argsA := tierClause("aa.privacy_tier", ceiling)
	tierB, argsB := tierClause("ab.privacy_tier", ceiling)
	args := append(append(append(append([]any{}, argsA...), argsB...), status), limit)
	rows, err := r.rq.QueryContext(ctx,
		`SELECT cf.id, cf.claim_a, cf.claim_b, cf.status, cf.rationale, cf.created_at FROM conflicts cf
		 JOIN claims ca ON ca.id = cf.claim_a
		 JOIN artifacts aa ON aa.id = ca.artifact_id AND aa.deleted_at IS NULL AND `+tierA+`
		 JOIN claims cb ON cb.id = cf.claim_b
		 JOIN artifacts ab ON ab.id = cb.artifact_id AND ab.deleted_at IS NULL AND `+tierB+`
		 WHERE cf.status = ? ORDER BY cf.created_at DESC, cf.id DESC LIMIT ?`,
		args...)
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
