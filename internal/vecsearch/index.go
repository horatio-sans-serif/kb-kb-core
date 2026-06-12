// Package vecsearch is the optional sqlite-vec (vec0) approximate-nearest-neighbor
// index over artifact embeddings (design §6.5). It replaces search's O(n) brute-force
// cosine scan with a sublinear KNN lookup at scale.
//
// The vec0 virtual table is a LOCAL, rebuilt index (never synced) keyed by the
// artifact ULID, holding the latest embedding per artifact. It is gated behind the
// `vec0` build tag because it compiles the native sqlite-vec extension; a default
// build has no vec0 and search falls back to the brute-force scan. When built with
// the tag, the daemon enables it (sqlite_vec.Auto, before the pools open) and search
// uses it; both degrade gracefully when no index is present.
package vecsearch

import (
	"context"
	"errors"

	"kb/internal/kb"
)

// ErrUnavailable means vec0 is not compiled into this binary (the default build).
var ErrUnavailable = errors.New("vecsearch: sqlite-vec not available in this build")

// Hit is one nearest-neighbor result: an artifact and its cosine similarity to the
// query (higher is better, matching the brute-force search score).
type Hit struct {
	ArtifactID string
	Score      float64
}

// EmbeddingSource streams the latest embedding (raw JSON payload) per live artifact —
// the source of truth the index is rebuilt from. *store.EnrichmentRepo satisfies it.
type EmbeddingSource interface {
	ForEachLatestEmbedding(ctx context.Context, fn func(artifactID, payload string) error) error
	CountByKind(ctx context.Context, kind string) (int, error)
}

// Index is the embedding ANN index. The concrete implementation is build-tag gated
// (vec0.go vs stub.go); search holds an Index that may be nil (no index — brute-force).
type Index interface {
	// Search returns up to k live artifacts nearest to qvec, best (most similar)
	// first. Tombstoned artifacts and those above the read ceiling (§8.17) are
	// filtered out.
	Search(ctx context.Context, qvec []float32, k int, ceiling kb.PrivacyTier) ([]Hit, error)
	// Reconcile rebuilds the index from src, in one transaction, when the embedding
	// population has changed since the last rebuild (a cheap count guard makes a
	// no-change reconcile a single COUNT query). Returns how many vectors it indexed
	// (0 when skipped). Maintenance is reconcile-based, not incremental, because the
	// embed stage runs inside the pipeline's write transaction and cannot open its own.
	Reconcile(ctx context.Context, src EmbeddingSource) (int, error)
}
