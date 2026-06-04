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

// TestSync_PreservesValueTypes converges a row with a REAL column set, a REAL column
// left NULL, and text — exercising the Value storage-class round-trip (a TEXT must
// not become a BLOB, a REAL must stay a REAL, a NULL must stay NULL) through the JSON
// transport with the real extension.
func TestSync_PreservesValueTypes(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)

	const id = "01ARTIFACT0000000000000002"
	// relevance_score (REAL) set to 0.75; content_hash (TEXT) left NULL.
	if _, err := dbA.ExecContext(ctx,
		`INSERT INTO artifacts(id, type, title, relevance_score, first_seen_at, last_seen_at, created_at, updated_at)
		 VALUES (?, 'note', 'typed', 0.75, 'ts','ts','ts','ts')`, id); err != nil {
		t.Fatalf("insert on A: %v", err)
	}

	srvA := httptest.NewServer(http.StripPrefix("/sync", Handler(DBChangeStore{DB: dbA}, testToken, discardLogger())))
	defer srvA.Close()
	p := NewPuller(DBChangeStore{DB: dbB}, newFakeWatermarks(), []string{srvA.URL}, testToken, srvA.Client(), discardLogger())
	p.SyncAll(ctx)

	var typ, title string
	var score *float64
	var contentHash *string
	if err := dbB.QueryRowContext(ctx,
		`SELECT type, title, relevance_score, content_hash FROM artifacts WHERE id = ?`, id).
		Scan(&typ, &title, &score, &contentHash); err != nil {
		t.Fatalf("row did not converge onto B: %v", err)
	}
	// type is a CHECK column: had the TEXT become a BLOB, the row would not exist.
	if typ != "note" || title != "typed" {
		t.Errorf("text columns wrong: type=%q title=%q", typ, title)
	}
	if score == nil || *score != 0.75 {
		t.Errorf("relevance_score = %v, want 0.75 (REAL preserved)", score)
	}
	if contentHash != nil {
		t.Errorf("content_hash = %v, want NULL preserved", *contentHash)
	}
}

// TestSync_Paginates exercises the multi-page pull loop: with a page size of one
// db_version, several commits on A still all converge onto B.
func TestSync_Paginates(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)

	const n = 5
	for i := 0; i < n; i++ {
		id := "01ARTIFACTPAGINATE000000" + string(rune('A'+i)) + "0"
		if _, err := dbA.ExecContext(ctx,
			`INSERT INTO artifacts(id, type, title, first_seen_at, last_seen_at, created_at, updated_at)
			 VALUES (?, 'note', 'p', 'ts','ts','ts','ts')`, id); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// One db_version per page forces the puller to loop across all commits.
	srvA := httptest.NewServer(http.StripPrefix("/sync", Handler(pagedStore{db: dbA, page: 1}, testToken, discardLogger())))
	defer srvA.Close()
	p := NewPuller(DBChangeStore{DB: dbB}, newFakeWatermarks(), []string{srvA.URL}, testToken, srvA.Client(), discardLogger())
	p.SyncAll(ctx)

	var got int
	if err := dbB.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Errorf("B has %d artifacts after paginated sync, want %d", got, n)
	}
}

// pagedStore serves Changes with a small page size to force pagination in tests.
type pagedStore struct {
	db   *sql.DB
	page int
}

func (s pagedStore) Changes(ctx context.Context, since int64) ([]Change, int64, error) {
	return Changes(ctx, s.db, since, s.page)
}
func (s pagedStore) Apply(ctx context.Context, changes []Change) error {
	return DBChangeStore{DB: s.db}.Apply(ctx, changes)
}
