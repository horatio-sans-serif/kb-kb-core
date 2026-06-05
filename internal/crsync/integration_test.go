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

// pull replicates one direction: every change on `from` is pulled into `to` (a fresh
// watermark, so it pulls the full history — safe and idempotent).
func pull(t *testing.T, from, to *sql.DB) {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/sync", Handler(DBChangeStore{DB: from}, testToken, discardLogger())))
	defer srv.Close()
	p := NewPuller(DBChangeStore{DB: to}, newFakeWatermarks(), []string{srv.URL}, testToken, srv.Client(), discardLogger())
	p.SyncAll(context.Background())
}

func insertArtifact(t *testing.T, db *sql.DB, id, title string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artifacts(id, type, title, first_seen_at, last_seen_at, created_at, updated_at)
		 VALUES (?, 'note', ?, 'ts','ts','ts','ts')`, id, title); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func title(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRowContext(context.Background(), `SELECT title FROM artifacts WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read title %s: %v", id, err)
	}
	return s
}

// TestSync_CompactionPropagates: the production compaction path (store.HardDelete of
// a tombstoned artifact) replicates as a clean delete — a row B already has is
// removed on B after sync, with no resurrection.
func TestSync_CompactionPropagates(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)
	stA := store.New(dbA)
	const id = "01COMPACT00000000000000ZZ"

	insertArtifact(t, dbA, id, "doomed")
	pull(t, dbA, dbB) // B has the live row

	// Purge (tombstone) then hard-delete on A — the compaction sequence.
	if _, err := dbA.ExecContext(ctx,
		`UPDATE artifacts SET deleted_at = '2026-02-02T00:00:00.000000000Z', lifecycle_tier = 'purged' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := stA.Artifacts.HardDelete(ctx, id); err != nil {
		t.Fatalf("hard-delete on A: %v", err)
	}
	pull(t, dbA, dbB)

	var nB int
	if err := dbB.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, id).Scan(&nB); err != nil {
		t.Fatal(err)
	}
	if nB != 0 {
		t.Errorf("hard-delete did not propagate to B (B still has %d rows)", nB)
	}

	// B re-syncing back to A does not resurrect it.
	pull(t, dbB, dbA)
	var nA int
	if err := dbA.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, id).Scan(&nA); err != nil {
		t.Fatal(err)
	}
	if nA != 0 {
		t.Errorf("compacted artifact resurrected on A (%d rows)", nA)
	}
}

// TestSync_DeleteVsConcurrentEditConverges: A hard-deletes a row while B (offline)
// edits the same row's title. After exchanging both ways, both replicas agree — the
// permanent-delete edge must still converge (no split-brain), per cr-sqlite's
// delete-vs-update causal resolution.
func TestSync_DeleteVsConcurrentEditConverges(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)
	stA := store.New(dbA)
	const id = "01DELVSEDIT0000000000000ZZ"

	insertArtifact(t, dbA, id, "orig")
	pull(t, dbA, dbB) // both have it

	// Concurrent: A purges+hard-deletes; B edits the title.
	if _, err := dbA.ExecContext(ctx,
		`UPDATE artifacts SET deleted_at = '2026-02-02T00:00:00.000000000Z', lifecycle_tier = 'purged' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := stA.Artifacts.HardDelete(ctx, id); err != nil {
		t.Fatalf("hard-delete on A: %v", err)
	}
	if _, err := dbB.ExecContext(ctx, `UPDATE artifacts SET title = 'edited-on-B' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	pull(t, dbA, dbB)
	pull(t, dbB, dbA)
	// And once more each way to let any resurrection settle deterministically.
	pull(t, dbA, dbB)
	pull(t, dbB, dbA)

	var nA, nB int
	dbA.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, id).Scan(&nA)
	dbB.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, id).Scan(&nB)
	if nA != nB {
		t.Errorf("replicas diverged after delete-vs-edit: A has %d, B has %d (must converge)", nA, nB)
	}
	if nA == 1 { // if a resurrection won, the row must be identical on both sides
		if ta, tb := title(t, dbA, id), title(t, dbB, id); ta != tb {
			t.Errorf("resurrected row diverged: A title=%q B title=%q", ta, tb)
		}
	}
}

// TestSync_CompactMixedTablesPropagates: a hard-delete whose transaction spans both
// CRR tables (artifacts, claims, conflicts, encounters) and local tables (jobs,
// audit_log) succeeds and the artifact delete still propagates to a peer.
func TestSync_CompactMixedTablesPropagates(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)
	stA := store.New(dbA)
	const id = "01MIXED00000000000000000ZZ"

	insertArtifact(t, dbA, id, "doomed")
	exec := func(q string, args ...any) {
		if _, err := dbA.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// CRR-table children and local-table children, all referencing the artifact.
	exec(`INSERT INTO sources(id,kind,connector) VALUES ('s1','PULL','rss')`)
	exec(`INSERT INTO encounters(id,artifact_id,source_id,seen_at) VALUES ('e1',?, 's1','ts')`, id)
	exec(`INSERT INTO claims(id,artifact_id,statement) VALUES ('c1',?, 'k')`, id)
	exec(`INSERT INTO jobs(id,type,artifact_id,status) VALUES ('j1','fetch',?, 'pending')`, id)   // local
	exec(`INSERT INTO audit_log(id,actor,action,artifact_id) VALUES ('a1','local','read',?)`, id) // local
	pull(t, dbA, dbB)

	exec(`UPDATE artifacts SET deleted_at = '2026-02-02T00:00:00.000000000Z', lifecycle_tier = 'purged' WHERE id = ?`, id)
	if err := stA.Artifacts.HardDelete(ctx, id); err != nil {
		t.Fatalf("hard-delete spanning CRR + local tables: %v", err)
	}
	// Local-table rows are gone locally.
	for _, tbl := range []string{"jobs", "audit_log"} {
		var n int
		dbA.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tbl+` WHERE artifact_id = ?`, id).Scan(&n)
		if n != 0 {
			t.Errorf("local %s row not deleted (%d)", tbl, n)
		}
	}
	pull(t, dbA, dbB)
	var nB int
	dbB.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE id = ?`, id).Scan(&nB)
	if nB != 0 {
		t.Errorf("artifact delete did not propagate to B (%d)", nB)
	}
}

// TestSync_Bidirectional: A writes X, B writes Y; after exchanging both ways, both
// replicas hold both rows.
func TestSync_Bidirectional(t *testing.T) {
	ext := extPath(t)
	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)
	insertArtifact(t, dbA, "01XXXXXXXXXXXXXXXXXXXXXXXX", "x")
	insertArtifact(t, dbB, "01YYYYYYYYYYYYYYYYYYYYYYYY", "y")

	pull(t, dbA, dbB) // A -> B
	pull(t, dbB, dbA) // B -> A

	for _, db := range []*sql.DB{dbA, dbB} {
		var n int
		if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM artifacts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("replica has %d artifacts, want 2 (both X and Y)", n)
		}
	}
}

// TestSync_DeleteConverges: a soft-delete (tombstone) on A converges onto B. Under
// cr-sqlite a tombstone is a normal change (the deleted_at column), so it replicates.
func TestSync_DeleteConverges(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)
	const id = "01DELETE0000000000000000ZZ"
	insertArtifact(t, dbA, id, "doomed")
	pull(t, dbA, dbB)

	// Soft-delete on A (the production tombstone — deleted_at set, row kept).
	if _, err := dbA.ExecContext(ctx,
		`UPDATE artifacts SET deleted_at = '2026-02-02T00:00:00.000000000Z', updated_at = '2026-02-02T00:00:00.000000000Z' WHERE id = ?`, id); err != nil {
		t.Fatalf("soft-delete on A: %v", err)
	}
	pull(t, dbA, dbB)

	var deletedAt *string
	if err := dbB.QueryRowContext(ctx, `SELECT deleted_at FROM artifacts WHERE id = ?`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("read B: %v", err)
	}
	if deletedAt == nil {
		t.Error("tombstone did not converge onto B (deleted_at still NULL)")
	}
}

// TestSync_ConcurrentEditConverges: A and B edit the same column offline, then
// exchange. cr-sqlite's LWW (column-version, then site id) makes both converge on the
// same deterministic value — the property the test asserts (it does not care which
// side wins, only that they agree).
func TestSync_ConcurrentEditConverges(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	dbA := openCRR(t, ext)
	dbB := openCRR(t, ext)
	const id = "01CONFLICT0000000000000000"
	insertArtifact(t, dbA, id, "orig")
	pull(t, dbA, dbB) // both have the row

	// Concurrent edits to the same column on each replica.
	if _, err := dbA.ExecContext(ctx, `UPDATE artifacts SET title = 'from-A' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := dbB.ExecContext(ctx, `UPDATE artifacts SET title = 'from-B' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	pull(t, dbA, dbB) // A -> B (B merges)
	pull(t, dbB, dbA) // B -> A (A adopts the merged winner)

	if a, b := title(t, dbA, id), title(t, dbB, id); a != b {
		t.Errorf("replicas diverged after concurrent edit: A=%q B=%q (LWW must converge)", a, b)
	}
}
