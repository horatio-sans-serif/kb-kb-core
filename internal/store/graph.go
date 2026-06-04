package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"kb/internal/kb"
)

// Entity and Link are defined in internal/kb (aliased in types.go).
type GraphRepo struct{ q, rq Querier } // q: write pool; rq: read pool

// ErrInvalidEntityKind / ErrInvalidRelation reject out-of-vocabulary graph writes
// (e.g. an LLM extractor emitting a kind the schema doesn't allow) with a typed
// error, before the raw CHECK constraint fires.
var (
	ErrInvalidEntityKind = errors.New("store: invalid entity kind")
	ErrInvalidRelation   = errors.New("store: invalid link relation")
)

// UpsertEntity inserts the entity or returns the id of the existing one with the
// same (kind, name) — the dedup keystone of the graph. Non-empty Aliases replace
// the stored set; nil Aliases leave an existing set intact. Returns the canonical
// entity id.
//
// Dedup is application-level (find-then-insert, atomic via withTx), not a
// UNIQUE(kind, name) constraint, which cr-sqlite (§8.16) forbids on a replicated
// table. A cross-replica duplicate (two devices coining the same entity offline)
// may transiently exist and is reconciled by ReconcileDuplicates.
func (r *GraphRepo) UpsertEntity(ctx context.Context, e *kb.Entity) (string, error) {
	if !kb.ValidEntityKind(e.Kind) {
		return "", fmt.Errorf("%w: %q", ErrInvalidEntityKind, e.Kind)
	}
	if e.ID == "" {
		e.ID = NewID()
	}
	var aliases any
	if len(e.Aliases) > 0 {
		b, err := json.Marshal(e.Aliases)
		if err != nil {
			return "", fmt.Errorf("marshal aliases: %w", err)
		}
		aliases = string(b)
	}
	var id string
	err := withTx(ctx, r.q, func(q Querier) error {
		var existing, existingAliases sql.NullString
		err := q.QueryRowContext(ctx,
			`SELECT id, aliases FROM entities WHERE kind = ? AND name = ? ORDER BY id LIMIT 1`,
			e.Kind, e.Name).Scan(&existing, &existingAliases)
		switch {
		case err == nil:
			// Re-mint of a known entity: keep its id, replace aliases only when new
			// ones are supplied (COALESCE semantics of the old upsert).
			id = existing.String
			if aliases != nil {
				if _, err := q.ExecContext(ctx,
					`UPDATE entities SET aliases = ? WHERE id = ?`, aliases, id); err != nil {
					return fmt.Errorf("update entity aliases %q: %w", id, err)
				}
			}
			return nil
		case errors.Is(err, sql.ErrNoRows):
			id = e.ID
			if _, err := q.ExecContext(ctx,
				`INSERT INTO entities(id, kind, name, aliases) VALUES (?, ?, ?, ?)`,
				e.ID, e.Kind, e.Name, aliases); err != nil {
				return fmt.Errorf("insert entity %q/%q: %w", e.Kind, e.Name, err)
			}
			return nil
		default:
			return fmt.Errorf("find entity %q/%q: %w", e.Kind, e.Name, err)
		}
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// ReconcileDuplicates merges cross-replica duplicate entities — rows sharing
// (kind, name) — into the lowest-id survivor (deterministic across replicas): it
// re-points the loser's artifact_entities to the survivor (UPDATE OR IGNORE +
// drop-leftover, since (artifact_id, entity_id) is the PK) and deletes the loser
// entity. Returns how many losers it merged. Each merge runs in its own transaction.
func (r *GraphRepo) ReconcileDuplicates(ctx context.Context) (int, error) {
	groups, err := r.duplicateEntityGroups(ctx)
	if err != nil {
		return 0, err
	}
	merged := 0
	var errs []error
	for _, g := range groups {
		survivor := g[0] // lowest id (groups come id-ascending)
		for _, loser := range g[1:] {
			err := withTx(ctx, r.q, func(q Querier) error {
				return (&GraphRepo{q: q, rq: q}).mergeEntity(ctx, survivor, loser)
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("merge entity %q into %q: %w", loser, survivor, err))
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

// duplicateEntityGroups returns the id groups (2+, id-ascending) of entities sharing
// a (kind, name). Boundaries are detected on the key columns directly.
func (r *GraphRepo) duplicateEntityGroups(ctx context.Context) ([][]string, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT kind, name, id FROM entities
		 WHERE (kind, name) IN (
		   SELECT kind, name FROM entities GROUP BY kind, name HAVING COUNT(*) > 1)
		 ORDER BY kind, name, id`)
	if err != nil {
		return nil, fmt.Errorf("duplicate entity groups: %w", err)
	}
	defer rows.Close()

	var out [][]string
	var curKind, curName string
	var cur []string
	first := true
	for rows.Next() {
		var kind, name, id string
		if err := rows.Scan(&kind, &name, &id); err != nil {
			return nil, fmt.Errorf("scan entity dup row: %w", err)
		}
		if first || kind != curKind || name != curName {
			if len(cur) > 1 {
				out = append(out, cur)
			}
			curKind, curName, cur, first = kind, name, nil, false
		}
		cur = append(cur, id)
	}
	if len(cur) > 1 {
		out = append(out, cur)
	}
	return out, rows.Err()
}

// mergeEntity folds the loser entity into the survivor; r must be transaction-bound.
func (r *GraphRepo) mergeEntity(ctx context.Context, survivor, loser string) error {
	if _, err := r.q.ExecContext(ctx,
		`UPDATE OR IGNORE artifact_entities SET entity_id = ? WHERE entity_id = ?`, survivor, loser); err != nil {
		return fmt.Errorf("re-point artifact_entities: %w", err)
	}
	if _, err := r.q.ExecContext(ctx,
		`DELETE FROM artifact_entities WHERE entity_id = ?`, loser); err != nil {
		return fmt.Errorf("drop leftover artifact_entities: %w", err)
	}
	if _, err := r.q.ExecContext(ctx, `DELETE FROM entities WHERE id = ?`, loser); err != nil {
		return fmt.Errorf("delete loser entity %q: %w", loser, err)
	}
	return nil
}

// LinkArtifactEntity records that an artifact mentions an entity. Idempotent.
func (r *GraphRepo) LinkArtifactEntity(ctx context.Context, artifactID, entityID string) error {
	if _, err := r.q.ExecContext(ctx,
		`INSERT OR IGNORE INTO artifact_entities(artifact_id, entity_id) VALUES (?, ?)`,
		artifactID, entityID); err != nil {
		return fmt.Errorf("link artifact %q entity %q: %w", artifactID, entityID, err)
	}
	return nil
}

// EntitiesForArtifact returns the entities an artifact mentions, by name.
func (r *GraphRepo) EntitiesForArtifact(ctx context.Context, artifactID string) ([]*kb.Entity, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT e.id, e.kind, e.name, e.aliases FROM entities e
		 JOIN artifact_entities ae ON ae.entity_id = e.id
		 WHERE ae.artifact_id = ? ORDER BY e.name, e.id`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("entities for artifact %q: %w", artifactID, err)
	}
	defer rows.Close()

	var out []*kb.Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ArtifactsForEntity returns the live artifact ids that mention an entity — the
// basis for shares_entity edges. Tombstoned artifacts are excluded.
func (r *GraphRepo) ArtifactsForEntity(ctx context.Context, entityID string) ([]string, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT ae.artifact_id FROM artifact_entities ae
		 JOIN artifacts a ON a.id = ae.artifact_id AND a.deleted_at IS NULL
		 WHERE ae.entity_id = ? ORDER BY ae.artifact_id`, entityID)
	if err != nil {
		return nil, fmt.Errorf("artifacts for entity %q: %w", entityID, err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// EntityWithCount is an entity plus how many live artifacts mention it.
type EntityWithCount struct {
	Entity        *kb.Entity
	ArtifactCount int
}

// ListEntities returns the entities mentioned by at least one live artifact, most-
// mentioned first — the global entity index.
func (r *GraphRepo) ListEntities(ctx context.Context, limit int) ([]EntityWithCount, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT e.id, e.kind, e.name, e.aliases, COUNT(*) AS n FROM entities e
		 JOIN artifact_entities ae ON ae.entity_id = e.id
		 JOIN artifacts a ON a.id = ae.artifact_id AND a.deleted_at IS NULL
		 GROUP BY e.id, e.kind, e.name, e.aliases ORDER BY n DESC, e.name LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	defer rows.Close()

	var out []EntityWithCount
	for rows.Next() {
		var e kb.Entity
		var aliases sql.NullString
		var n int
		if err := rows.Scan(&e.ID, &e.Kind, &e.Name, &aliases, &n); err != nil {
			return nil, fmt.Errorf("scan entity count: %w", err)
		}
		if aliases.Valid && aliases.String != "" {
			if err := json.Unmarshal([]byte(aliases.String), &e.Aliases); err != nil {
				return nil, fmt.Errorf("unmarshal entity %q aliases: %w", e.ID, err)
			}
		}
		out = append(out, EntityWithCount{Entity: &e, ArtifactCount: n})
	}
	return out, rows.Err()
}

// GetEntity returns an entity by id, or ErrNotFound.
func (r *GraphRepo) GetEntity(ctx context.Context, id string) (*kb.Entity, error) {
	e, err := scanEntity(r.rq.QueryRowContext(ctx, `SELECT id, kind, name, aliases FROM entities WHERE id = ?`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get entity %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get entity %q: %w", id, err)
	}
	return e, nil
}

// ArtifactRef is an artifact's display fields, joined into a graph query so the
// caller needs no per-row lookup.
type ArtifactRef struct {
	ID    string
	Title string
	URL   string
}

const graphDefaultLimit = 50

// EntityArtifacts returns up to limit live artifacts that mention an entity, with
// their display fields (id, title, url), oldest first.
func (r *GraphRepo) EntityArtifacts(ctx context.Context, entityID string, limit int) ([]ArtifactRef, error) {
	if limit <= 0 {
		limit = graphDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT a.id, a.title, a.canonical_url FROM artifact_entities ae
		 JOIN artifacts a ON a.id = ae.artifact_id AND a.deleted_at IS NULL
		 WHERE ae.entity_id = ? ORDER BY a.id LIMIT ?`, entityID, limit)
	if err != nil {
		return nil, fmt.Errorf("artifacts for entity %q: %w", entityID, err)
	}
	defer rows.Close()
	return scanArtifactRefs(rows)
}

// Backlink is one incoming edge: a live artifact that links TO the queried one,
// under a relation. The same source appears once per relation (a source linking
// under two relations yields two backlinks).
type Backlink struct {
	ArtifactRef
	Relation string
}

// Backlinks returns up to limit live artifacts that link to artifactID (incoming
// edges) with their display fields — the reverse-direction graph query (§8.7).
func (r *GraphRepo) Backlinks(ctx context.Context, artifactID string, limit int) ([]Backlink, error) {
	if limit <= 0 {
		limit = graphDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT DISTINCT l.src_id, a.title, a.canonical_url, l.relation FROM links l
		 JOIN artifacts a ON a.id = l.src_id AND a.deleted_at IS NULL
		 WHERE l.dst_id = ? ORDER BY l.src_id, l.relation LIMIT ?`, artifactID, limit)
	if err != nil {
		return nil, fmt.Errorf("backlinks for %q: %w", artifactID, err)
	}
	defer rows.Close()

	var out []Backlink
	for rows.Next() {
		var b Backlink
		var title, u sql.NullString
		if err := rows.Scan(&b.ID, &title, &u, &b.Relation); err != nil {
			return nil, fmt.Errorf("scan backlink: %w", err)
		}
		b.Title, b.URL = title.String, u.String
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanArtifactRefs(rows *sql.Rows) ([]ArtifactRef, error) {
	var out []ArtifactRef
	for rows.Next() {
		var ref ArtifactRef
		var title, u sql.NullString
		if err := rows.Scan(&ref.ID, &title, &u); err != nil {
			return nil, fmt.Errorf("scan artifact ref: %w", err)
		}
		ref.Title, ref.URL = title.String, u.String
		out = append(out, ref)
	}
	return out, rows.Err()
}

// AddLink records a directed edge, deduped on (src, dst, relation). Idempotent.
func (r *GraphRepo) AddLink(ctx context.Context, src, dst, relation string, weight float64) error {
	if !kb.ValidRelation(relation) {
		return fmt.Errorf("%w: %q", ErrInvalidRelation, relation)
	}
	// Dedup is application-level (find-then-insert), not a unique index: cr-sqlite
	// (§8.16) forbids non-primary-key unique indexes on replicated tables. Atomic
	// via withTx so concurrent callers never double-insert (a cross-replica
	// duplicate is reconciled by DedupLinks, not prevented here).
	return withTx(ctx, r.q, func(q Querier) error {
		var exists bool
		if err := q.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM links WHERE src_id = ? AND dst_id = ? AND relation = ?)`,
			src, dst, relation).Scan(&exists); err != nil {
			return fmt.Errorf("find link %q->%q (%s): %w", src, dst, relation, err)
		}
		if exists {
			return nil
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO links(id, src_id, dst_id, relation, weight, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			NewID(), src, dst, relation, weight, now()); err != nil {
			return fmt.Errorf("add link %q->%q (%s): %w", src, dst, relation, err)
		}
		return nil
	})
}

// DedupLinks collapses cross-replica duplicate links — rows sharing
// (src_id, dst_id, relation) — keeping the lowest id (deterministic across
// replicas). Links carry no references, so the extras are simply dropped. Returns
// how many it removed.
func (r *GraphRepo) DedupLinks(ctx context.Context) (int, error) {
	var n int64
	err := withTx(ctx, r.q, func(q Querier) error {
		res, err := q.ExecContext(ctx,
			`DELETE FROM links WHERE id NOT IN (SELECT MIN(id) FROM links GROUP BY src_id, dst_id, relation)`)
		if err != nil {
			return fmt.Errorf("dedup links: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

// neighborChunk bounds how many frontier ids go into one query. Each query uses
// 2× this many bind variables (src and dst IN lists), kept well under SQLite's
// ~32k variable ceiling so a wide frontier (a hub node) cannot fail the query.
const neighborChunk = 500

// maxConnectedNodes bounds a single ConnectedWithin traversal so a pathological
// (dense/huge) graph cannot make one query expand without limit.
const maxConnectedNodes = 5000

// NeighborIDs returns the distinct artifact ids adjacent to any id in the frontier,
// treating edges as undirected (an edge in either direction is a neighbor). Live
// artifacts only. The frontier is chunked so an arbitrarily wide frontier never
// exceeds the bind-variable ceiling.
func (r *GraphRepo) NeighborIDs(ctx context.Context, frontier []string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	for start := 0; start < len(frontier); start += neighborChunk {
		end := start + neighborChunk
		if end > len(frontier) {
			end = len(frontier)
		}
		ids, err := r.neighborsOf(ctx, frontier[start:end])
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// neighborsOf runs the adjacency query for one bounded chunk of the frontier.
func (r *GraphRepo) neighborsOf(ctx context.Context, chunk []string) ([]string, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	ph := make([]string, len(chunk))
	args := make([]any, 0, len(chunk)*2)
	for i, id := range chunk {
		ph[i] = "?"
		args = append(args, id)
	}
	in := strings.Join(ph, ", ")
	args = append(args, args...) // src-side and dst-side IN lists bind the same ids
	q := `SELECT DISTINCT n FROM (
	        SELECT dst_id AS n FROM links WHERE src_id IN (` + in + `)
	        UNION
	        SELECT src_id AS n FROM links WHERE dst_id IN (` + in + `)
	      ) JOIN artifacts a ON a.id = n AND a.deleted_at IS NULL
	      ORDER BY n`
	rows, err := r.rq.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("neighbors: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// ConnectedWithin returns the artifact ids reachable from start within hops edges
// (undirected), excluding start itself, sorted. A breadth-first expansion bounded
// by hops; the visited set prevents cycles, and the traversal stops once
// maxConnectedNodes have been reached so a hub node cannot expand without limit.
func (r *GraphRepo) ConnectedWithin(ctx context.Context, start string, hops int) ([]string, error) {
	visited := map[string]bool{start: true}
	frontier := []string{start}
	for h := 0; h < hops && len(frontier) > 0 && len(visited) < maxConnectedNodes; h++ {
		nbrs, err := r.NeighborIDs(ctx, frontier)
		if err != nil {
			return nil, err
		}
		var next []string
		for _, n := range nbrs {
			if !visited[n] {
				visited[n] = true
				next = append(next, n)
				if len(visited) >= maxConnectedNodes {
					break
				}
			}
		}
		frontier = next
	}
	out := make([]string, 0, len(visited)-1)
	for id := range visited {
		if id != start {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

func scanEntity(sc scanner) (*kb.Entity, error) {
	var e kb.Entity
	var aliases sql.NullString
	if err := sc.Scan(&e.ID, &e.Kind, &e.Name, &aliases); err != nil {
		return nil, fmt.Errorf("scan entity: %w", err)
	}
	if aliases.Valid && aliases.String != "" {
		if err := json.Unmarshal([]byte(aliases.String), &e.Aliases); err != nil {
			return nil, fmt.Errorf("unmarshal aliases for %q: %w", e.ID, err)
		}
	}
	return &e, nil
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
