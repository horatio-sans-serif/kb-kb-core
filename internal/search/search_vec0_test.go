//go:build vec0

package search

import (
	"context"
	"path/filepath"
	"testing"

	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/vecsearch"
)

// TestSemantic_RealVec0Index is the B1 end-to-end check: the Searcher backed by a
// real sqlite-vec ANN index returns the nearest live artifact first.
func TestSemantic_RealVec0Index(t *testing.T) {
	ctx := context.Background()
	vecsearch.Enable() // before opening the pools
	path := filepath.Join(t.TempDir(), "kb.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Separate read pool, as in production: Reconcile's write transaction streams the
	// embeddings from the read pool, so the two must not share the single connection.
	rdb, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read pool: %v", err)
	}
	defer rdb.Close()
	st := store.NewWithReadPool(db, rdb)

	near := seedNoteWithEmbedding(t, st, "hn", "near", "near body", []float32{1, 0, 0})
	// far is less similar but still positively correlated (cosine 0.6), so it is a
	// match ranked below near (an orthogonal vector would be filtered as a non-match).
	far := seedNoteWithEmbedding(t, st, "hf", "far", "far body", []float32{0.6, 0.8, 0})

	idx, err := vecsearch.New(db, rdb, 3)
	if err != nil {
		t.Fatalf("new index: %v", err)
	}
	if _, err := idx.Reconcile(ctx, st.Enrichments); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	s := NewSearcher(st, queryEmbedder{vec: []float32{1, 0, 0}}, "m", true)
	s.SetIndex(idx)

	res, err := s.Semantic(ctx, "q", 2, kb.Restricted)
	if err != nil {
		t.Fatalf("semantic: %v", err)
	}
	if len(res) != 2 || res[0].ArtifactID != near || res[1].ArtifactID != far {
		t.Fatalf("results = %+v, want near (%s) before far (%s)", res, near, far)
	}
	if res[0].Score <= res[1].Score {
		t.Errorf("scores not descending: %+v", res)
	}
}
