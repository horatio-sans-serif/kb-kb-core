//go:build vec0

package vecsearch

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"kb/internal/kb"
	"kb/internal/store"
)

// fakeSource feeds the index a fixed set of latest embeddings.
type fakeSource struct {
	rows  map[string][]float32
	count int
}

func (f *fakeSource) ForEachLatestEmbedding(_ context.Context, fn func(id, payload string) error) error {
	for id, v := range f.rows {
		b, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: len(v), Vector: v})
		if err := fn(id, string(b)); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeSource) CountByKind(_ context.Context, _ string) (int, error) { return f.count, nil }

// openWithVec returns distinct write and read pools (as New requires).
func openWithVec(t *testing.T) (write, read *sql.DB) {
	t.Helper()
	Enable() // before opening the pools, so their connections carry the extension
	path := filepath.Join(t.TempDir(), "kb.db")
	w, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if _, err := store.Migrate(w); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	r, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return w, r
}

func rawArtifact(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artifacts(id, type, first_seen_at, last_seen_at) VALUES (?, 'note', 'ts', 'ts')`, id); err != nil {
		t.Fatalf("seed artifact %s: %v", id, err)
	}
}

func TestVecIndex_ReconcileAndSearchNearestLiveFirst(t *testing.T) {
	ctx := context.Background()
	w, r := openWithVec(t)
	for _, id := range []string{"01A", "01B", "01C"} {
		rawArtifact(t, w, id)
	}
	ix, err := New(w, r, 3)
	if err != nil {
		t.Fatalf("new index: %v", err)
	}
	src := &fakeSource{count: 3, rows: map[string][]float32{
		"01A": {1, 0, 0}, "01B": {0, 1, 0}, "01C": {0.9, 0.1, 0},
	}}
	n, err := ix.Reconcile(ctx, src)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 3 {
		t.Errorf("indexed %d, want 3", n)
	}

	hits, err := ix.Search(ctx, []float32{1, 0, 0}, 2, kb.Restricted)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 || hits[0].ArtifactID != "01A" || hits[1].ArtifactID != "01C" {
		t.Fatalf("hits = %+v, want 01A then 01C (nearest by cosine)", hits)
	}
	if hits[0].Score <= hits[1].Score || hits[0].Score < 0.99 {
		t.Errorf("scores wrong (want descending, nearest ~1): %+v", hits)
	}

	// Tombstone the nearest; it drops out of the live-filtered results.
	if _, err := w.ExecContext(ctx, `UPDATE artifacts SET deleted_at = 'now' WHERE id = '01A'`); err != nil {
		t.Fatal(err)
	}
	hits, _ = ix.Search(ctx, []float32{1, 0, 0}, 2, kb.Restricted)
	for _, h := range hits {
		if h.ArtifactID == "01A" {
			t.Errorf("tombstoned 01A returned by search: %+v", hits)
		}
	}
	if len(hits) == 0 || hits[0].ArtifactID != "01C" {
		t.Errorf("after tombstone, nearest live = %+v, want 01C first", hits)
	}
}

func TestNew_RejectsSamePool(t *testing.T) {
	w, _ := openWithVec(t)
	if _, err := New(w, w, 3); err == nil {
		t.Error("New must reject write == read (Reconcile would deadlock the single writer)")
	}
	if _, err := New(w, w, 0); err == nil {
		t.Error("New must reject a non-positive dimension")
	}
}

func TestVecIndex_SearchCeiling(t *testing.T) {
	ctx := context.Background()
	w, r := openWithVec(t)
	exec := func(q string) {
		if _, err := w.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO artifacts(id, type, privacy_tier, first_seen_at, last_seen_at) VALUES ('01A','note','public','ts','ts')`)
	exec(`INSERT INTO artifacts(id, type, privacy_tier, first_seen_at, last_seen_at) VALUES ('01B','note','sensitive','ts','ts')`)
	ix, err := New(w, r, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Both vectors are the nearest to the query; the ceiling decides who is returned.
	if _, err := ix.Reconcile(ctx, &fakeSource{count: 2, rows: map[string][]float32{
		"01A": {1, 0, 0}, "01B": {1, 0, 0},
	}}); err != nil {
		t.Fatal(err)
	}

	pub, _ := ix.Search(ctx, []float32{1, 0, 0}, 5, kb.Public)
	if len(pub) != 1 || pub[0].ArtifactID != "01A" {
		t.Errorf("public ceiling = %+v, want only the public artifact 01A", pub)
	}
	owner, _ := ix.Search(ctx, []float32{1, 0, 0}, 5, kb.Restricted)
	if len(owner) != 2 {
		t.Errorf("owner ceiling = %d hits, want both", len(owner))
	}
}

func TestVecIndex_ReconcileCountGuard(t *testing.T) {
	ctx := context.Background()
	w, r := openWithVec(t)
	rawArtifact(t, w, "01A")
	ix, err := New(w, r, 3)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{count: 1, rows: map[string][]float32{"01A": {1, 0, 0}}}
	if n, _ := ix.Reconcile(ctx, src); n != 1 {
		t.Errorf("first reconcile indexed %d, want 1", n)
	}
	// Same count → skipped (no rebuild).
	if n, _ := ix.Reconcile(ctx, src); n != 0 {
		t.Errorf("unchanged reconcile indexed %d, want 0 (count guard)", n)
	}
	// Count changed → rebuilds.
	src.count = 2
	rawArtifact(t, w, "01B")
	src.rows["01B"] = []float32{0, 1, 0}
	if n, _ := ix.Reconcile(ctx, src); n != 2 {
		t.Errorf("changed reconcile indexed %d, want 2", n)
	}
}

func TestVecIndex_ReconcileSkipsBadDim(t *testing.T) {
	ctx := context.Background()
	w, r := openWithVec(t)
	rawArtifact(t, w, "01A")
	rawArtifact(t, w, "01B")
	ix, err := New(w, r, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 01B has a wrong-dimension vector and must be skipped, not fail the rebuild.
	src := &fakeSource{count: 2, rows: map[string][]float32{"01A": {1, 0, 0}, "01B": {1, 1}}}
	n, err := ix.Reconcile(ctx, src)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Errorf("indexed %d, want 1 (the bad-dim vector skipped)", n)
	}
}
