//go:build vec0

package vecsearch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"kb/internal/kb"
)

// liveBuffer is how many extra neighbors the KNN fetches beyond k, so tombstoned
// artifacts filtered out by the live-join still leave k live results in the common
// case (a few stale neighbors). Capped scan, still sublinear.
const liveBuffer = 32

// Enable registers sqlite-vec so every SQLite connection opened afterward has the
// vec0 extension. It MUST be called before the database pools are opened.
func Enable() { sqlite_vec.Auto() }

// vecIndex is the vec0-backed ANN index. write maintains the table (Reconcile); read
// serves queries (Search) over the same WAL file.
type vecIndex struct {
	write *sql.DB
	read  *sql.DB
	dim   int

	mu        sync.Mutex
	lastCount int // embedding-row count at the last rebuild (change guard)
}

// New creates (if needed) the vec0 table and returns the index. dim is the embedding
// dimension (must match the embed model). The pools must have been opened after
// Enable so their connections carry the extension. write and read MUST be distinct
// pools: Reconcile opens a write transaction and streams the embeddings from the read
// pool concurrently, which would deadlock on a single-connection pool shared by both.
func New(write, read *sql.DB, dim int) (Index, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("vecsearch: invalid embedding dimension %d", dim)
	}
	ix := &vecIndex{write: write, read: read, dim: dim}
	// cosine distance so 1-distance is the cosine similarity the brute-force scan
	// reports; a fixed FLOAT[dim] vector column keyed by the artifact ULID.
	if _, err := write.ExecContext(context.Background(), fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS artifact_vectors USING vec0(
		   artifact_id TEXT PRIMARY KEY,
		   embedding FLOAT[%d] distance_metric=cosine)`, dim)); err != nil {
		return nil, fmt.Errorf("vecsearch: create vec0 table: %w", err)
	}
	return ix, nil
}

// Reconcile rebuilds the index from src in one transaction when the embedding
// population has changed since the last rebuild. The cheap count guard makes a
// no-change reconcile a single COUNT query, so it is fine to call on a ticker.
func (ix *vecIndex) Reconcile(ctx context.Context, src EmbeddingSource) (int, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	count, err := src.CountByKind(ctx, "embedding")
	if err != nil {
		return 0, fmt.Errorf("vecsearch: count embeddings: %w", err)
	}
	if count == ix.lastCount {
		return 0, nil // nothing changed since the last rebuild
	}

	tx, err := ix.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("vecsearch: begin reconcile: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifact_vectors`); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("vecsearch: clear index: %w", err)
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO artifact_vectors(artifact_id, embedding) VALUES (?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("vecsearch: prepare insert: %w", err)
	}
	defer insert.Close()

	indexed := 0
	err = src.ForEachLatestEmbedding(ctx, func(artifactID, payload string) error {
		var ep kb.EmbeddingPayload
		if json.Unmarshal([]byte(payload), &ep) != nil || len(ep.Vector) != ix.dim {
			return nil // corrupt or wrong-dimension (stale model) — skip, as search does
		}
		b, err := sqlite_vec.SerializeFloat32(ep.Vector)
		if err != nil {
			return err
		}
		if _, err := insert.ExecContext(ctx, artifactID, b); err != nil {
			return err
		}
		indexed++
		return nil
	})
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("vecsearch: reconcile rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("vecsearch: commit reconcile: %w", err)
	}
	ix.lastCount = count
	return indexed, nil
}

func (ix *vecIndex) Search(ctx context.Context, qvec []float32, k int) ([]Hit, error) {
	if k <= 0 {
		return nil, nil
	}
	if len(qvec) != ix.dim {
		return nil, fmt.Errorf("vecsearch: query dimension %d != index dimension %d", len(qvec), ix.dim)
	}
	b, err := sqlite_vec.SerializeFloat32(qvec)
	if err != nil {
		return nil, fmt.Errorf("vecsearch: serialize query: %w", err)
	}
	// The KNN runs in a subquery (pure vec0 scan of the nearest k+buffer), then the
	// outer query joins to artifacts to drop tombstoned hits. The buffer compensates
	// for those drops so k live results remain in the common case.
	rows, err := ix.read.QueryContext(ctx,
		`SELECT knn.artifact_id, knn.distance
		 FROM (
		   SELECT artifact_id, distance FROM artifact_vectors
		   WHERE embedding MATCH ? ORDER BY distance LIMIT ?
		 ) AS knn
		 JOIN artifacts a ON a.id = knn.artifact_id AND a.deleted_at IS NULL
		 ORDER BY knn.distance
		 LIMIT ?`, b, k+liveBuffer, k)
	if err != nil {
		return nil, fmt.Errorf("vecsearch: knn query: %w", err)
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var id string
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, fmt.Errorf("vecsearch: scan hit: %w", err)
		}
		// cosine distance -> cosine similarity. Skip non-positive similarity, as the
		// brute-force scan does (an unrelated/anti-correlated artifact is not a match).
		if score := 1 - dist; score > 0 {
			out = append(out, Hit{ArtifactID: id, Score: score})
		}
	}
	return out, rows.Err()
}
