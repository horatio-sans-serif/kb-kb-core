package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrate_FreshDB(t *testing.T) {
	db := openTemp(t)

	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}

	for _, tbl := range []string{"sources", "artifacts", "encounters", "enrichments", "schema_migrations"} {
		if !tableExists(t, db, tbl) {
			t.Errorf("table %q missing after migrate", tbl)
		}
	}

	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != 1 {
		t.Errorf("schema version = %d, want 1", v)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openTemp(t)

	if _, err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if applied != 0 {
		t.Errorf("second run applied = %d, want 0", applied)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", n)
	}
}

func TestMigrate_PragmasSet(t *testing.T) {
	db := openTemp(t)
	if _, err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1 (on)", fk)
	}
}

// --- helpers ---

func openTemp(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kb.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("tableExists(%q): %v", name, err)
	}
	return got == name
}
