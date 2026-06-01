package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStore_ReadPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.db")
	wdb, err := Open(path)
	if err != nil {
		t.Fatalf("open write: %v", err)
	}
	t.Cleanup(func() { _ = wdb.Close() })
	if _, err := Migrate(wdb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rdb, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	st := NewWithReadPool(wdb, rdb)
	ctx := context.Background()

	// Write via the write pool; the read-pool Get must see the committed row (WAL).
	a, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "note", Title: "x"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.Artifacts.Get(ctx, a.ID) // Get routes to the read pool
	if err != nil || got.ID != a.ID {
		t.Fatalf("read pool did not see the write: got=%v err=%v", got, err)
	}

	// The read pool is genuinely read-only.
	if _, err := rdb.ExecContext(ctx,
		`INSERT INTO sources(id, kind, connector) VALUES ('x', 'PULL', 'y')`); err == nil {
		t.Error("expected the read-only pool to reject a write")
	}
}
