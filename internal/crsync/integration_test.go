//go:build crsqlite

package crsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"database/sql"

	"kb/internal/store"
)

// openCRR opens a fresh migrated database with the cr-sqlite extension loaded and
// every synced table converted to a CRR — a real replica.
func openCRR(t *testing.T, ext string) *sql.DB {
	t.Helper()
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
	t.Cleanup(func() { _ = Finalize(ctx, db); _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := AsCRR(ctx, db, SyncedTables()); err != nil {
		t.Fatalf("as_crr: %v", err)
	}
	return db
}

// TestSync_EndToEnd_TwoReplicas proves the full stack: an artifact written on
// replica A is pulled over the HTTP transport (DBChangeStore + Handler + Puller) and
// converges on replica B, using the real extension.
func TestSync_EndToEnd_TwoReplicas(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()

	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)

	// Write an artifact on A (a CRR-table write produces a changeset).
	const id = "01ARTIFACT0000000000000001"
	if _, err := dbA.ExecContext(ctx,
		`INSERT INTO artifacts(id, type, title, first_seen_at, last_seen_at, created_at, updated_at)
		 VALUES (?, 'note', 'hello from A', '2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z')`,
		id); err != nil {
		t.Fatalf("insert on A: %v", err)
	}

	// A serves its changes; B pulls them.
	srvA := httptest.NewServer(http.StripPrefix("/sync", Handler(DBChangeStore{DB: dbA}, testToken, discardLogger())))
	defer srvA.Close()

	wm := newFakeWatermarks()
	p := NewPuller(DBChangeStore{DB: dbB}, wm, []string{srvA.URL}, testToken, srvA.Client(), discardLogger())
	p.SyncAll(ctx)

	// The artifact converged onto B with its title.
	var title string
	if err := dbB.QueryRowContext(ctx, `SELECT title FROM artifacts WHERE id = ?`, id).Scan(&title); err != nil {
		t.Fatalf("artifact did not converge onto B: %v", err)
	}
	if title != "hello from A" {
		t.Errorf("converged title = %q, want %q", title, "hello from A")
	}

	// The watermark advanced past 0 (B recorded how far it pulled from A).
	if v, _ := wm.Watermark(ctx, srvA.URL); v <= 0 {
		t.Errorf("watermark = %d, want > 0 after pulling a change", v)
	}
}
