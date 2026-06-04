//go:build crsqlite

package crsync

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"

	"kb/internal/store"
)

// TestAsCRR_AllSyncedTables is the Stage 2c acceptance test: after migration 0013
// the real cr-sqlite extension must accept every synced table as a CRR. This is the
// definition of done for the cr-sqlite-compatible schema — if a future migration
// re-introduces a nullable PK, a non-PK unique index, or a checked foreign key on a
// synced table, crsql_as_crr fails here.
//
// Run with:
//
//	KB_CRSQLITE_PATH=/path/crsqlite.dylib go test -tags 'sqlite_fts5 crsqlite' \
//	  -run TestAsCRR_AllSyncedTables ./internal/crsync/
func TestAsCRR_AllSyncedTables(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "kb.db")
	q := url.Values{
		"_foreign_keys": {"on"},
		"_journal_mode": {"WAL"},
		"_busy_timeout": {"5000"},
		"_txlock":       {"immediate"},
	}
	db, err := Open(ext, "file:"+path+"?"+q.Encode())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = Finalize(ctx, db); _ = db.Close() }()

	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := AsCRR(ctx, db, SyncedTables()); err != nil {
		t.Fatalf("crsql_as_crr must accept every synced table after 0013: %v", err)
	}

	// The local-only tables are correctly NOT converted (a sanity check that the
	// classification, not a blanket conversion, is what passed). artifacts_fts is a
	// virtual table, for which crsql_as_crr is not meaningful.
	for _, tbl := range LocalTables() {
		if tbl == "artifacts_fts" {
			continue
		}
		if _, err := db.ExecContext(ctx, `SELECT crsql_as_crr(?)`, tbl); err == nil {
			t.Errorf("local-only table %q should not be a CRR, but crsql_as_crr accepted it", tbl)
		}
	}
}
