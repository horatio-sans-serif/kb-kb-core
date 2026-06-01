package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestMigrate_FreshDB(t *testing.T) {
	db := openTemp(t)

	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if applied < 1 {
		t.Fatalf("applied = %d, want >= 1", applied)
	}

	for _, tbl := range []string{"sources", "artifacts", "encounters", "enrichments", "schema_migrations"} {
		if !tableExists(t, db, tbl) {
			t.Errorf("table %q missing after migrate", tbl)
		}
	}

	// Every applied migration is recorded exactly once (count-robust as
	// migrations accumulate across phases).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != applied {
		t.Errorf("recorded %d migrations, want %d (one row per applied)", n, applied)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openTemp(t)

	first, err := Migrate(db)
	if err != nil {
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
	if n != first {
		t.Errorf("schema_migrations rows = %d, want %d (no duplicate on re-run)", n, first)
	}
}

func TestVersionFromName_Invalid(t *testing.T) {
	for _, name := range []string{"", "noseparator.sql", "abc_desc.sql", "0_zero.sql", "_x.sql"} {
		if _, err := versionFromName(name); err == nil {
			t.Errorf("versionFromName(%q): expected an error", name)
		}
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

// TestMigrate_PartialFailureIsRecoverable proves a migration that fails partway
// leaves the database at the previous version with none of its statements
// persisted, and that fixing the migration and re-running recovers cleanly.
func TestMigrate_PartialFailureIsRecoverable(t *testing.T) {
	db := openTemp(t)

	broken := fstest.MapFS{
		"0001_good.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id TEXT PRIMARY KEY);`)},
		"0002_bad.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE b (id TEXT PRIMARY KEY);\n" +
				"INSERT INTO no_such_table(x) VALUES ('boom');")},
	}

	applied, err := migrateFS(db, broken)
	if err == nil {
		t.Fatalf("expected error from broken migration, got nil (applied=%d)", applied)
	}
	if applied != 1 {
		t.Errorf("applied = %d, want 1 (only the good migration committed)", applied)
	}

	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != 1 {
		t.Errorf("version = %d, want 1 (failed migration must not advance it)", v)
	}
	if tableExists(t, db, "b") {
		t.Error("table b persisted; the failed migration was not rolled back")
	}
	if !tableExists(t, db, "a") {
		t.Error("table a missing; the good migration should have committed")
	}

	fixed := fstest.MapFS{
		"0001_good.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id TEXT PRIMARY KEY);`)},
		"0002_bad.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE b (id TEXT PRIMARY KEY);`)},
	}
	applied, err = migrateFS(db, fixed)
	if err != nil {
		t.Fatalf("re-run after fix: %v", err)
	}
	if applied != 1 {
		t.Errorf("re-run applied = %d, want 1 (the fixed migration)", applied)
	}
	if !tableExists(t, db, "b") {
		t.Error("table b missing after the fixed re-run")
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
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("tableExists(%q): %v", name, err)
	}
	return got == name
}
