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

func openWithVec(t *testing.T) *sql.DB {
	t.Helper()
	Enable() // before opening the pool, so its connections carry the extension
	db, err := store.Open(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
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
	db := openWithVec(t)
	for _, id := range []string{"01A", "01B", "01C"} {
		rawArtifact(t, db, id)
	}
	ix, err := New(db, db, 3)
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

	hits, err := ix.Search(ctx, []float32{1, 0, 0}, 2)
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
	if _, err := db.ExecContext(ctx, `UPDATE artifacts SET deleted_at = 'now' WHERE id = '01A'`); err != nil {
		t.Fatal(err)
	}
	hits, _ = ix.Search(ctx, []float32{1, 0, 0}, 2)
	for _, h := range hits {
		if h.ArtifactID == "01A" {
			t.Errorf("tombstoned 01A returned by search: %+v", hits)
		}
	}
	if len(hits) == 0 || hits[0].ArtifactID != "01C" {
		t.Errorf("after tombstone, nearest live = %+v, want 01C first", hits)
	}
}

func TestVecIndex_ReconcileCountGuard(t *testing.T) {
	ctx := context.Background()
	db := openWithVec(t)
	rawArtifact(t, db, "01A")
	ix, err := New(db, db, 3)
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
	rawArtifact(t, db, "01B")
	src.rows["01B"] = []float32{0, 1, 0}
	if n, _ := ix.Reconcile(ctx, src); n != 2 {
		t.Errorf("changed reconcile indexed %d, want 2", n)
	}
}

func TestVecIndex_ReconcileSkipsBadDim(t *testing.T) {
	ctx := context.Background()
	db := openWithVec(t)
	rawArtifact(t, db, "01A")
	rawArtifact(t, db, "01B")
	ix, err := New(db, db, 3)
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
