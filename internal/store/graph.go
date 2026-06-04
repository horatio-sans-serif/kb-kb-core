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
	err := r.q.QueryRowContext(ctx,
		`INSERT INTO entities(id, kind, name, aliases) VALUES (?, ?, ?, ?)
		 ON CONFLICT(kind, name) DO UPDATE SET aliases = COALESCE(excluded.aliases, entities.aliases)
		 RETURNING id`,
		e.ID, e.Kind, e.Name, aliases).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("upsert entity %q/%q: %w", e.Kind, e.Name, err)
	}
	return id, nil
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

// Backlink is one incoming edge: a live artifact that links TO the queried one.
type Backlink struct {
	SrcID    string
	Relation string
}

// Backlinks returns the live artifacts that link to artifactID (incoming edges) —
// the reverse-direction graph query (§8.7).
func (r *GraphRepo) Backlinks(ctx context.Context, artifactID string) ([]Backlink, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT DISTINCT l.src_id, l.relation FROM links l
		 JOIN artifacts a ON a.id = l.src_id AND a.deleted_at IS NULL
		 WHERE l.dst_id = ? ORDER BY l.src_id, l.relation`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("backlinks for %q: %w", artifactID, err)
	}
	defer rows.Close()

	var out []Backlink
	for rows.Next() {
		var b Backlink
		if err := rows.Scan(&b.SrcID, &b.Relation); err != nil {
			return nil, fmt.Errorf("scan backlink: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddLink records a directed edge, deduped on (src, dst, relation). Idempotent.
func (r *GraphRepo) AddLink(ctx context.Context, src, dst, relation string, weight float64) error {
	if !kb.ValidRelation(relation) {
		return fmt.Errorf("%w: %q", ErrInvalidRelation, relation)
	}
	if _, err := r.q.ExecContext(ctx,
		`INSERT OR IGNORE INTO links(id, src_id, dst_id, relation, weight, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		NewID(), src, dst, relation, weight, now()); err != nil {
		return fmt.Errorf("add link %q->%q (%s): %w", src, dst, relation, err)
	}
	return nil
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
