//go:build crsqlite

package crsync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// extPath returns the cr-sqlite extension path from KB_CRSQLITE_PATH, skipping the
// test when it is unset or missing — the engine is only exercised where the native
// extension is available.
func extPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("KB_CRSQLITE_PATH")
	if p == "" {
		t.Skip("KB_CRSQLITE_PATH not set; skipping cr-sqlite engine test")
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("cr-sqlite extension not found at %q", p)
	}
	return p
}

func openReplica(t *testing.T, ext, name string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ext, "file:"+filepath.Join(t.TempDir(), name)+"?_foreign_keys=off")
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	db.SetMaxOpenConns(1) // keep one connection so the extension stays loaded + finalize works
	t.Cleanup(func() { _ = Finalize(ctx, db); _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE note (id TEXT NOT NULL PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatalf("create on %s: %v", name, err)
	}
	if err := AsCRR(ctx, db, []string{"note"}); err != nil {
		t.Fatalf("as_crr on %s: %v", name, err)
	}
	return db
}

func TestEngine_DistinctRowsConverge(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	a := openReplica(t, ext, "a.db")
	b := openReplica(t, ext, "b.db")

	if _, err := a.ExecContext(ctx, `INSERT INTO note (id, body) VALUES ('A1', 'from A')`); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if _, err := b.ExecContext(ctx, `INSERT INTO note (id, body) VALUES ('B1', 'from B')`); err != nil {
		t.Fatalf("insert B: %v", err)
	}

	// Exchange changesets both directions (ULID-keyed rows never collide on PK).
	ca, _, err := Changes(ctx, a, 0)
	if err != nil {
		t.Fatalf("changes A: %v", err)
	}
	cb, _, err := Changes(ctx, b, 0)
	if err != nil {
		t.Fatalf("changes B: %v", err)
	}
	if err := Apply(ctx, b, ca); err != nil {
		t.Fatalf("apply A->B: %v", err)
	}
	if err := Apply(ctx, a, cb); err != nil {
		t.Fatalf("apply B->A: %v", err)
	}

	for _, r := range []struct {
		name string
		db   *sql.DB
	}{{"A", a}, {"B", b}} {
		var n int
		if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM note`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", r.name, err)
		}
		if n != 2 {
			t.Errorf("replica %s has %d rows, want 2 (both replicas' rows after sync)", r.name, n)
		}
		var body string
		if err := r.db.QueryRowContext(ctx, `SELECT body FROM note WHERE id='B1'`).Scan(&body); err != nil || body != "from B" {
			t.Errorf("replica %s missing B's row: body=%q err=%v", r.name, body, err)
		}
	}
}

func TestEngine_WatermarkIncremental(t *testing.T) {
	ext := extPath(t)
	ctx := context.Background()
	a := openReplica(t, ext, "a.db")
	b := openReplica(t, ext, "b.db")

	_, _ = a.ExecContext(ctx, `INSERT INTO note (id, body) VALUES ('A1', 'one')`)
	ca, vA, err := Changes(ctx, a, 0)
	if err != nil || len(ca) == 0 {
		t.Fatalf("first changes: %v (%d rows)", err, len(ca))
	}
	_ = Apply(ctx, b, ca)

	// A new local change advances A's version; pulling since the watermark yields
	// only the new change.
	_, _ = a.ExecContext(ctx, `INSERT INTO note (id, body) VALUES ('A2', 'two')`)
	ca2, _, err := Changes(ctx, a, vA)
	if err != nil {
		t.Fatalf("incremental changes: %v", err)
	}
	for _, c := range ca2 {
		if c.DBVersion <= vA {
			t.Errorf("incremental pull returned a change at/below the watermark: %d <= %d", c.DBVersion, vA)
		}
	}
	_ = Apply(ctx, b, ca2)
	var n int
	_ = b.QueryRowContext(ctx, `SELECT COUNT(*) FROM note`).Scan(&n)
	if n != 2 {
		t.Errorf("B after incremental sync has %d rows, want 2", n)
	}
}
