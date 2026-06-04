package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Artifact is defined in internal/kb (aliased in types.go).
type ArtifactRepo struct{ q, rq Querier } // q: write pool; rq: read pool

const artifactColNames = `id, type, canonical_url, content_hash, title, body_markdown,
	privacy_tier, lifecycle_tier, status, relevance_score,
	first_seen_at, last_seen_at, last_engaged_at, deleted_at, created_at, updated_at`

const artifactSelect = `SELECT ` + artifactColNames + ` FROM artifacts`

// UpsertByCanonical inserts the artifact, or — when a live row already has the
// same canonical URL — bumps its last_seen_at/updated_at and returns it. This is
// the one-artifact-many-encounters keystone (Principle 1).
//
// Dedup is application-level (find-the-live-row-then-insert), not a DB unique
// constraint: cr-sqlite (§8.16) forbids non-primary-key unique indexes on
// replicated tables, because uniqueness can't be enforced across offline replicas.
// So a cross-replica duplicate may transiently exist and is reconciled by the
// same_as merge sweep, not prevented here. The find and the insert are atomic: when
// already inside a Tx the caller's immediate transaction serializes them (the
// ingest path); called directly it opens its own immediate transaction (see inTx),
// so a single replica never makes a duplicate either way. A nil CanonicalURL never
// dedups (always inserts). The find is scoped to live rows, so a tombstoned URL is
// free for a new artifact (a re-sighting after soft-delete is a new artifact). A
// re-sighting only touches last_seen_at/updated_at — never an existing artifact's
// enriched fields.
func (r *ArtifactRepo) UpsertByCanonical(ctx context.Context, a *Artifact) (*Artifact, error) {
	return r.upsert(ctx, a, "canonical_url", a.CanonicalURL)
}

// UpsertByContentHash is the content-fingerprint dedup path (§8.2): the same
// content provided as a body, or fetched from different URLs, collapses to one
// artifact. ContentHash must be set. URL artifacts carry a NULL content_hash until
// fetched, so the two upsert paths never collide. Same find-then-insert semantics
// as UpsertByCanonical.
func (r *ArtifactRepo) UpsertByContentHash(ctx context.Context, a *Artifact) (*Artifact, error) {
	return r.upsert(ctx, a, "content_hash", a.ContentHash)
}

// inTx runs fn against an ArtifactRepo bound to a transaction. If this repo is
// already transaction-scoped (q is a *sql.Tx, e.g. inside Store.Tx) it reuses that
// transaction; otherwise (q is the *sql.DB write pool, an auto-commit call) it
// opens its own immediate transaction. This makes a multi-statement invariant — the
// find-then-insert upsert, the merge's re-points — atomic for a direct caller too,
// not only when wrapped in Store.Tx. _txlock=immediate means BEGIN takes the write
// lock, so concurrent writers serialize rather than race.
func (r *ArtifactRepo) inTx(ctx context.Context, fn func(*ArtifactRepo) error) error {
	db, ok := r.q.(*sql.DB)
	if !ok {
		return fn(r) // already inside a transaction
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifact tx begin: %w", err)
	}
	if err := fn(&ArtifactRepo{q: tx, rq: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifact tx commit: %w", err)
	}
	return nil
}

// upsert runs the find-then-insert atomically (see inTx), then delegates to
// upsertLocked. keyCol is a trusted constant column name ("canonical_url" or
// "content_hash"), not user input.
func (r *ArtifactRepo) upsert(ctx context.Context, a *Artifact, keyCol string, key *string) (*Artifact, error) {
	var out *Artifact
	err := r.inTx(ctx, func(tr *ArtifactRepo) error {
		var e error
		out, e = tr.upsertLocked(ctx, a, keyCol, key)
		return e
	})
	return out, err
}

// upsertLocked finds a live artifact already carrying the dedup key and touches it,
// or inserts a new one. It MUST run with r bound to a transaction (upsert ensures
// this via inTx) so the find and the insert are one atomic unit.
func (r *ArtifactRepo) upsertLocked(ctx context.Context, a *Artifact, keyCol string, key *string) (*Artifact, error) {
	if key != nil {
		existing, err := r.findLiveByKey(ctx, keyCol, *key)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			ts := now()
			if _, err := r.q.ExecContext(ctx,
				`UPDATE artifacts SET last_seen_at = ?, updated_at = ? WHERE id = ?`, ts, ts, existing.ID); err != nil {
				return nil, fmt.Errorf("touch artifact %q: %w", existing.ID, err)
			}
			existing.LastSeenAt, existing.UpdatedAt = ts, ts
			return existing, nil
		}
	}
	return r.insert(ctx, a)
}

// findLiveByKey returns the live artifact carrying key in keyCol, or nil.
func (r *ArtifactRepo) findLiveByKey(ctx context.Context, keyCol, key string) (*Artifact, error) {
	a, err := scanArtifact(r.q.QueryRowContext(ctx,
		artifactSelect+` WHERE `+keyCol+` = ? AND deleted_at IS NULL LIMIT 1`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find artifact by %s: %w", keyCol, err)
	}
	return a, nil
}

// insert writes a new artifact (applying defaults) and indexes it.
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
		 RETURNING `+artifactColNames,
		a.ID, a.Type, a.CanonicalURL, a.ContentHash, a.Title, a.BodyMarkdown,
		a.PrivacyTier, a.LifecycleTier, a.Status, a.RelevanceScore,
		a.FirstSeenAt, a.LastSeenAt, ts, ts)

	got, err := scanArtifact(row)
	if err != nil {
		return nil, fmt.Errorf("insert artifact: %w", err)
	}
	if err := r.reindexFTS(ctx, got.ID); err != nil {
		return nil, err
	}
	return got, nil
}

// reindexFTS rebuilds the artifact's full-text row from its current live state.
// It reads via the write querier so it observes an update made earlier in the
// same transaction; a tombstoned or missing artifact is simply removed from the
// index (no row re-added). Called from every artifact write that can change the
// indexed title/body, so the FTS index stays consistent without DB triggers.
func (r *ArtifactRepo) reindexFTS(ctx context.Context, id string) error {
	if _, err := r.q.ExecContext(ctx, `DELETE FROM artifacts_fts WHERE artifact_id = ?`, id); err != nil {
		return fmt.Errorf("fts delete %q: %w", id, err)
	}
	var title, body sql.NullString
	err := r.q.QueryRowContext(ctx,
		`SELECT title, body_markdown FROM artifacts WHERE id = ? AND deleted_at IS NULL`, id).
		Scan(&title, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // tombstoned or gone — leave it out of the index
	}
	if err != nil {
		return fmt.Errorf("fts read %q: %w", id, err)
	}
	if _, err := r.q.ExecContext(ctx,
		`INSERT INTO artifacts_fts(artifact_id, title, body_markdown) VALUES (?, ?, ?)`,
		id, title.String, body.String); err != nil {
		return fmt.Errorf("fts insert %q: %w", id, err)
	}
	return nil
}

// KeywordHit is one full-text match: an artifact and its relevance score (higher
// is better — the FTS5 bm25 rank is negated, since bm25 returns more-negative for
// better matches).
type KeywordHit struct {
	ArtifactID string
	Score      float64
}

// SearchKeyword runs an FTS5 MATCH (match must be a valid FTS5 query — the search
// layer builds a safe one from user input) and returns live artifacts ranked by
// bm25, best first. The join to artifacts drops tombstoned rows whose stale FTS
// entries may linger.
func (r *ArtifactRepo) SearchKeyword(ctx context.Context, match string, limit int) ([]KeywordHit, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT f.artifact_id, bm25(artifacts_fts) AS rank
		 FROM artifacts_fts f
		 JOIN artifacts a ON a.id = f.artifact_id AND a.deleted_at IS NULL
		 WHERE artifacts_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()

	var out []KeywordHit
	for rows.Next() {
		var h KeywordHit
		var rank float64
		if err := rows.Scan(&h.ArtifactID, &rank); err != nil {
			return nil, fmt.Errorf("scan fts hit: %w", err)
		}
		h.Score = -rank // bm25: more negative = better; flip so higher = better
		out = append(out, h)
	}
	return out, rows.Err()
}

func (r *ArtifactRepo) Get(ctx context.Context, id string) (*Artifact, error) {
	a, err := scanArtifact(r.rq.QueryRowContext(ctx, artifactSelect+` WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get artifact %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get artifact %q: %w", id, err)
	}
	return a, nil
}

// GetLive is Get scoped to live (non-tombstoned) rows: it returns ErrNotFound for
// a soft-deleted artifact. Retrieval paths (search/RAG) use it so a delete that
// races ranking-then-load cannot resurface a tombstoned artifact's content.
func (r *ArtifactRepo) GetLive(ctx context.Context, id string) (*Artifact, error) {
	a, err := scanArtifact(r.rq.QueryRowContext(ctx, artifactSelect+` WHERE id = ? AND deleted_at IS NULL`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get live artifact %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get live artifact %q: %w", id, err)
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

// SurfaceQuery selects and ranks artifacts for the surfacing engine (§8.11):
// live, scored artifacts ordered by relevance (then recency), with optional
// filters. Tiers is a privacy whitelist (empty = all); Since bounds first_seen_at
// (new arrivals); Status filters the pipeline state ("" = any); MinScore floors
// the relevance score; Limit caps the result (<=0 applies a default cap).
type SurfaceQuery struct {
	Status   string
	Since    string
	MinScore float64
	Tiers    []string
	Limit    int
}

const surfaceDefaultLimit = 50

// Surface returns the ranked candidates for SurfaceQuery: live, scored artifacts
// ordered by relevance_score (desc), then recency (first_seen_at desc), then id.
// Only artifacts with a relevance score participate (an unscored, still-enriching
// artifact is not surfaced).
func (r *ArtifactRepo) Surface(ctx context.Context, q SurfaceQuery) ([]*Artifact, error) {
	where := []string{"deleted_at IS NULL", "relevance_score IS NOT NULL", "relevance_score >= ?"}
	args := []any{q.MinScore}
	if q.Status != "" {
		where = append(where, "status = ?")
		args = append(args, q.Status)
	}
	if q.Since != "" {
		where = append(where, "first_seen_at >= ?")
		args = append(args, q.Since)
	}
	if len(q.Tiers) > 0 {
		ph := make([]string, len(q.Tiers))
		for i, t := range q.Tiers {
			ph[i] = "?"
			args = append(args, t)
		}
		where = append(where, "privacy_tier IN ("+strings.Join(ph, ", ")+")")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = surfaceDefaultLimit
	}
	args = append(args, limit)

	query := artifactSelect + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY relevance_score DESC, first_seen_at DESC, id LIMIT ?"
	rows, err := r.rq.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("surface artifacts: %w", err)
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

func (r *ArtifactRepo) list(ctx context.Context, clause string) ([]*Artifact, error) {
	rows, err := r.rq.QueryContext(ctx, artifactSelect+clause)
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

// RecordEngagement stamps last_engaged_at (and updated_at) on a live artifact —
// the engagement signal the relevance model boosts (§8.9) — and returns it to the
// active lifecycle tier: engagement is the explicit "I care about this" that
// reverses an archive (the lifecycle sweep only demotes, never promotes, §8.12).
// It does not touch title/body, so no FTS reindex is needed. Reports whether a
// live row was updated (false = unknown or tombstoned artifact).
func (r *ArtifactRepo) RecordEngagement(ctx context.Context, id string) (bool, error) {
	ts := now()
	res, err := r.q.ExecContext(ctx,
		`UPDATE artifacts SET last_engaged_at = ?, updated_at = ?, lifecycle_tier = 'active'
		 WHERE id = ? AND deleted_at IS NULL`,
		ts, ts, id)
	if err != nil {
		return false, fmt.Errorf("record engagement for %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record engagement for %q (rows affected): %w", id, err)
	}
	return n > 0, nil
}

// EnrichingIDs returns the ids of live artifacts still in the 'enriching' status
// — the candidates for startup reconciliation (an artifact whose jobs are all
// terminal but whose finalize hook never ran, e.g. a crash between a job's commit
// and its post-completion finalize).
func (r *ArtifactRepo) EnrichingIDs(ctx context.Context) ([]string, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT id FROM artifacts WHERE status = 'enriching' AND deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("list enriching artifacts: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan enriching id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Merge folds the loser artifact into the survivor (the design's same_as dedup):
// it re-points the loser's child rows to the survivor — UPDATE OR IGNORE then drop
// any that would have duplicated an existing survivor row (a shared encounter
// source, entity, topic, …) — folds the loser's artifact-level signals into the
// survivor (earliest sighting, latest engagement, active tier), drops the loser's
// claims and their conflicts (the survivor keeps its own derived ones), and
// tombstones the loser. The caller picks the survivor deterministically (lowest id)
// so independent replicas converge on the same merge. Idempotent and live-scoped: a
// survivor==loser or already-merged loser is a no-op. Atomic via inTx — the
// reconcile sweep wraps it in a Tx; a direct caller is wrapped automatically.
//
// The loser's enrichments re-point too rather than being deleted, so the survivor
// may carry two of a kind (its own plus the loser's). That is intentional:
// enrichments is a synced table (no uniqueness possible), and every read path is
// latest-per-kind (LatestByKind / ForEachLatestEmbedding), so one wins and the
// redundant row is harmless dead weight. The child re-points DELETE leftover rows
// (those tables carry no deleted_at to tombstone); a concurrent cross-replica merge
// can therefore leave a residual duplicate child after sync — acceptable under the
// design's best-effort dedup (§8.16), and the next single-replica sweep is exact.
func (r *ArtifactRepo) Merge(ctx context.Context, survivorID, loserID string) error {
	if survivorID == loserID {
		return nil
	}
	return r.inTx(ctx, func(tr *ArtifactRepo) error {
		return tr.mergeLocked(ctx, survivorID, loserID)
	})
}

// mergeLocked is Merge's body; r must be transaction-bound (Merge ensures this).
func (r *ArtifactRepo) mergeLocked(ctx context.Context, survivorID, loserID string) error {
	reassign := func(table, col string) error {
		if _, err := r.q.ExecContext(ctx, `UPDATE OR IGNORE `+table+` SET `+col+` = ? WHERE `+col+` = ?`, survivorID, loserID); err != nil {
			return fmt.Errorf("merge re-point %s.%s: %w", table, col, err)
		}
		if _, err := r.q.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+` = ?`, loserID); err != nil {
			return fmt.Errorf("merge cleanup %s.%s: %w", table, col, err)
		}
		return nil
	}
	// Every table that references artifacts(id). engagements is the explicit "I
	// care" feedback signal (§8.9); jobs/audit_log are local-only but still point
	// at the artifact. renders.artifact_ids is a JSON array, not an FK column, so
	// the loser id lingers in past-render provenance — accepted (a render is a
	// historical record, not live state).
	for _, tc := range []struct{ table, col string }{
		{"encounters", "artifact_id"},
		{"enrichments", "artifact_id"},
		{"artifact_entities", "artifact_id"},
		{"artifact_topics", "artifact_id"},
		{"review_schedule", "artifact_id"},
		{"engagements", "artifact_id"},
		{"jobs", "artifact_id"},
		{"audit_log", "artifact_id"},
		{"links", "src_id"},
		{"links", "dst_id"},
	} {
		if err := reassign(tc.table, tc.col); err != nil {
			return err
		}
	}
	// Re-pointing a mutually-linked pair (both S->L and L->S existed — duplicates
	// share entities/topics, so the graph builder linked them both ways) collapses
	// both edges to one S->S self-loop. Drop them: a node is not its own neighbor.
	if _, err := r.q.ExecContext(ctx, `DELETE FROM links WHERE src_id = dst_id`); err != nil {
		return fmt.Errorf("merge drop self-loop links: %w", err)
	}
	// The loser's claims (and the conflicts referencing them) are dropped — the
	// survivor keeps its own; conflicts re-detect over the merged set.
	if _, err := r.q.ExecContext(ctx,
		`DELETE FROM conflicts WHERE claim_a IN (SELECT id FROM claims WHERE artifact_id = ?)
		    OR claim_b IN (SELECT id FROM claims WHERE artifact_id = ?)`, loserID, loserID); err != nil {
		return fmt.Errorf("merge loser conflicts: %w", err)
	}
	if _, err := r.q.ExecContext(ctx, `DELETE FROM claims WHERE artifact_id = ?`, loserID); err != nil {
		return fmt.Errorf("merge loser claims: %w", err)
	}
	if err := r.foldSignals(ctx, survivorID, loserID); err != nil {
		return err
	}
	ts := now()
	if _, err := r.q.ExecContext(ctx,
		`UPDATE artifacts SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		ts, ts, loserID); err != nil {
		return fmt.Errorf("merge tombstone loser %q: %w", loserID, err)
	}
	return r.reindexFTS(ctx, loserID) // drop the tombstoned loser from the FTS index
}

// foldSignals carries the loser's artifact-level aggregate signals into the
// survivor before the loser is tombstoned: the earliest first_seen_at, the latest
// last_seen_at and last_engaged_at, and the active tier if either side is active
// (engagement reverses an archive, §8.12). Without this the merge would discard the
// loser's sighting history and engagement — the most valuable user signal — when it
// tombstones the loser row those columns live on. TimeFormat strings are
// lexicographically ordered, so min/max are plain string comparisons.
func (r *ArtifactRepo) foldSignals(ctx context.Context, survivorID, loserID string) error {
	survivor, err := r.Get(ctx, survivorID)
	if err != nil {
		return fmt.Errorf("merge fold: read survivor: %w", err)
	}
	loser, err := r.Get(ctx, loserID)
	if err != nil {
		return fmt.Errorf("merge fold: read loser: %w", err)
	}
	firstSeen := minStr(survivor.FirstSeenAt, loser.FirstSeenAt)
	lastSeen := maxStr(survivor.LastSeenAt, loser.LastSeenAt)
	lastEngaged := maxPtr(survivor.LastEngagedAt, loser.LastEngagedAt)
	lifecycle := survivor.LifecycleTier
	if survivor.LifecycleTier == "active" || loser.LifecycleTier == "active" {
		lifecycle = "active"
	}
	if _, err := r.q.ExecContext(ctx,
		`UPDATE artifacts SET first_seen_at = ?, last_seen_at = ?, last_engaged_at = ?, lifecycle_tier = ?, updated_at = ?
		 WHERE id = ?`,
		firstSeen, lastSeen, lastEngaged, lifecycle, now(), survivorID); err != nil {
		return fmt.Errorf("merge fold signals into survivor %q: %w", survivorID, err)
	}
	return nil
}

// minStr/maxStr return the lexicographically smaller/larger of two TimeFormat
// strings (== chronological order, see store.TimeFormat).
func minStr(a, b string) string {
	if a < b {
		return a
	}
	return b
}

func maxStr(a, b string) string {
	if a > b {
		return a
	}
	return b
}

// maxPtr returns the later of two optional timestamps; nil (never set) sorts before
// any real timestamp, and nil only when both are nil.
func maxPtr(a, b *string) *string {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a >= *b:
		return a
	default:
		return b
	}
}

// DuplicateGroups returns the sets of live artifacts that share a dedup key (a
// canonical_url or a content_hash) — the cross-replica duplicates the reconcile
// sweep merges. Each group has 2+ members, id-ascending (so the caller's survivor
// is deterministic). A group never mixes the two key kinds.
func (r *ArtifactRepo) DuplicateGroups(ctx context.Context) ([][]string, error) {
	var out [][]string
	for _, keyCol := range []string{"canonical_url", "content_hash"} {
		groups, err := r.dupGroupsByKey(ctx, keyCol)
		if err != nil {
			return nil, err
		}
		out = append(out, groups...)
	}
	return out, nil
}

// dupGroupsByKey returns the id groups (2+, id-ascending) of live artifacts that
// share a value in keyCol (a trusted constant column name).
func (r *ArtifactRepo) dupGroupsByKey(ctx context.Context, keyCol string) ([][]string, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT `+keyCol+`, id FROM artifacts
		 WHERE `+keyCol+` IS NOT NULL AND deleted_at IS NULL
		   AND `+keyCol+` IN (
		     SELECT `+keyCol+` FROM artifacts
		     WHERE `+keyCol+` IS NOT NULL AND deleted_at IS NULL
		     GROUP BY `+keyCol+` HAVING COUNT(*) > 1)
		 ORDER BY `+keyCol+`, id`)
	if err != nil {
		return nil, fmt.Errorf("duplicate groups by %s: %w", keyCol, err)
	}
	defer rows.Close()
	return groupByKey(rows)
}

// groupByKey collapses (key, id) rows (ordered by key, id) into per-key id slices.
func groupByKey(rows *sql.Rows) ([][]string, error) {
	var out [][]string
	var curKey string
	var cur []string
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			return nil, fmt.Errorf("scan dup row: %w", err)
		}
		if key != curKey {
			if len(cur) > 1 {
				out = append(out, cur)
			}
			curKey, cur = key, nil
		}
		cur = append(cur, id)
	}
	if len(cur) > 1 {
		out = append(out, cur)
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

// Patch applies a narrow UPDATE of only the set fields of p (plus updated_at);
// a no-op when nothing is set. Enrichment stages use it to write their artifact
// effects (fetch sets the body, relevance sets the score) one column-narrow
// statement at a time (§6.0). Scoped to live rows: a patch must never resurrect
// a tombstoned artifact's columns (which would diverge from the merged delete
// under cr-sqlite).
func (r *ArtifactRepo) Patch(ctx context.Context, id string, p ArtifactPatch) error {
	sets := make([]string, 0, 5)
	args := make([]any, 0, 7)
	if p.Type != nil {
		sets, args = append(sets, "type = ?"), append(args, *p.Type)
	}
	if p.BodyMarkdown != nil {
		sets, args = append(sets, "body_markdown = ?"), append(args, *p.BodyMarkdown)
	}
	if p.ContentHash != nil {
		sets, args = append(sets, "content_hash = ?"), append(args, *p.ContentHash)
	}
	if p.Status != nil {
		sets, args = append(sets, "status = ?"), append(args, *p.Status)
	}
	if p.RelevanceScore != nil {
		sets, args = append(sets, "relevance_score = ?"), append(args, *p.RelevanceScore)
	}
	if p.LifecycleTier != nil {
		sets, args = append(sets, "lifecycle_tier = ?"), append(args, *p.LifecycleTier)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, now(), id)
	if _, err := r.q.ExecContext(ctx,
		`UPDATE artifacts SET `+strings.Join(sets, ", ")+` WHERE id = ? AND deleted_at IS NULL`, args...); err != nil {
		return fmt.Errorf("patch artifact %q: %w", id, err)
	}
	// The fetch stage patches body_markdown; re-index so the fetched content is
	// searchable (title is not patchable, so only a body change matters).
	if p.BodyMarkdown != nil {
		if err := r.reindexFTS(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// LifecycleCounts is the artifact population by lifecycle state.
type LifecycleCounts struct {
	Active  int
	Archive int
	Purged  int // tombstoned (deleted_at set)
}

// LifecycleCounts returns the live active/archive counts and the GC-purged count
// (tombstoned AND lifecycle_tier='purged', so a user/other-path tombstone is not
// miscounted as a GC purge) — the lifecycle status view. A full-table aggregate;
// fine for an on-demand endpoint at the personal scale this targets.
func (r *ArtifactRepo) LifecycleCounts(ctx context.Context) (LifecycleCounts, error) {
	var c LifecycleCounts
	err := r.rq.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN deleted_at IS NULL AND lifecycle_tier = 'active' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN deleted_at IS NULL AND lifecycle_tier = 'archive' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN deleted_at IS NOT NULL AND lifecycle_tier = 'purged' THEN 1 ELSE 0 END), 0)
		 FROM artifacts`).Scan(&c.Active, &c.Archive, &c.Purged)
	if err != nil {
		return LifecycleCounts{}, fmt.Errorf("lifecycle counts: %w", err)
	}
	return c, nil
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
