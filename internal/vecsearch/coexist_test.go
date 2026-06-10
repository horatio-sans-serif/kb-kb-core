//go:build vec0 && crsqlite

package vecsearch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kb/internal/crsync"
	"kb/internal/kb"
	"kb/internal/store"
)

// TestVec0AndCrsqliteCoexist runs the production configuration: the write pool
// carries BOTH the cr-sqlite driver and vec0 (sqlite_vec.Auto), the synced tables are
// converted to CRRs, and the vec0 index is built and queried. It asserts the vec0
// table and its shadow tables do not leak into the cr-sqlite changeset, and that
// crsql_finalize succeeds with vec0 loaded.
func TestVec0AndCrsqliteCoexist(t *testing.T) {
	ext := os.Getenv("KB_CRSQLITE_PATH")
	if ext == "" {
		t.Skip("KB_CRSQLITE_PATH not set; skipping vec0+cr-sqlite coexistence test")
	}
	ctx := context.Background()

	Enable() // vec0, before any pool opens
	driver, err := crsync.Register(ext)
	if err != nil {
		t.Skipf("cr-sqlite unavailable: %v", err)
	}

	path := filepath.Join(t.TempDir(), "kb.db")
	db, err := store.OpenWithDriver(path, driver) // write pool: cr-sqlite driver + vec0 auto
	if err != nil {
		t.Fatalf("open write: %v", err)
	}
	defer db.Close()
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := crsync.AsCRR(ctx, db, crsync.SyncedTables()); err != nil {
		t.Fatalf("as_crr: %v", err)
	}
	rdb, err := store.OpenReadOnly(path) // read pool: vec0 auto applies; cr-sqlite not needed for reads
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	defer rdb.Close()
	st := store.NewWithReadPool(db, rdb)

	// Seed an artifact (a CRR write) and its embedding.
	hash := "h1"
	a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: kb.Note, Title: "t", ContentHash: &hash})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: 3, Vector: []float32{1, 0, 0}})
	if err := st.Enrichments.Create(ctx, &kb.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(payload), Model: "m", PromptVersion: "v1"}); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}

	idx, err := New(db, rdb, 3)
	if err != nil {
		t.Fatalf("new vec0 index: %v", err)
	}
	if _, err := idx.Reconcile(ctx, st.Enrichments); err != nil {
		t.Fatalf("reconcile (vec0 writes alongside CRR tables): %v", err)
	}

	// The vec0 table and its shadow tables must NOT appear in the changeset.
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT "table" FROM crsql_changes`)
	if err != nil {
		t.Fatalf("read changes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		if !crsync.IsSynced(table) {
			t.Errorf("non-synced table %q leaked into crsql_changes (vec0 shadow table?)", table)
		}
	}

	// vec0 KNN still works on the read pool.
	hits, err := idx.Search(ctx, []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("vec0 search: %v", err)
	}
	if len(hits) != 1 || hits[0].ArtifactID != a.ID {
		t.Errorf("search = %+v, want the seeded artifact", hits)
	}

	// crsql_finalize must succeed with the vec0 module loaded.
	if err := crsync.Finalize(ctx, db); err != nil {
		t.Errorf("crsql_finalize with vec0 loaded: %v", err)
	}
}
