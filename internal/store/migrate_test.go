package store

import (
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"kb/migrations"
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

func TestOpenWithDriver(t *testing.T) {
	// The default driver name works (Open delegates to it) and migrates.
	path := filepath.Join(t.TempDir(), "kb.db")
	db, err := OpenWithDriver(path, "sqlite3")
	if err != nil {
		t.Fatalf("OpenWithDriver(sqlite3): %v", err)
	}
	defer db.Close()
	if _, err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !tableExists(t, db, "artifacts") {
		t.Error("artifacts missing after OpenWithDriver+Migrate")
	}

	// An unregistered driver fails at open.
	if _, err := OpenWithDriver(filepath.Join(t.TempDir(), "x.db"), "no_such_driver"); err == nil {
		t.Error("OpenWithDriver with an unregistered driver should error")
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

// TestMigration0013_PreservesData verifies the synced-table rebuild in 0013
// (create -> copy -> drop -> rename) carries data through. A fresh full migrate runs
// 0013 on empty tables and would hide a column omitted from the copy, so this seeds
// data after 0012 and then applies 0013.
func TestMigration0013_PreservesData(t *testing.T) {
	db := openTemp(t)

	// Apply only 0001..0012.
	upTo12 := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := versionFromName(e.Name())
		if err != nil {
			t.Fatalf("version %q: %v", e.Name(), err)
		}
		if v > 12 {
			continue
		}
		b, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatalf("read %q: %v", e.Name(), err)
		}
		upTo12[e.Name()] = &fstest.MapFile{Data: b}
	}
	if _, err := migrateFS(db, upTo12); err != nil {
		t.Fatalf("migrate to 0012: %v", err)
	}

	// Seed a parent (sources, with the 0005 columns), an artifact, and a child
	// (encounter) — all fully populated.
	ts := Now()
	if _, err := db.Exec(
		`INSERT INTO sources(id, kind, connector, cursor, last_fetched_at, health) VALUES ('s1','PULL','rss','cur',?, '{}')`, ts); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO artifacts(id, type, canonical_url, title, privacy_tier, lifecycle_tier, status, first_seen_at, last_seen_at, created_at, updated_at)
		 VALUES ('a1','webpage','https://x','T','public','active','ready',?,?,?,?)`, ts, ts, ts, ts); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO encounters(id, artifact_id, source_id, seen_at, created_at) VALUES ('e1','a1','s1',?,?)`, ts, ts); err != nil {
		t.Fatalf("seed encounter: %v", err)
	}
	// A jobs row referencing the artifact is the precondition that makes the
	// artifacts rebuild fail at COMMIT if migrations run with foreign keys enforced
	// (jobs keeps its FK to artifacts and is not rebuilt). This row guards the runner
	// disabling FK enforcement during migration.
	if _, err := db.Exec(
		`INSERT INTO jobs(id, type, artifact_id, status, created_at) VALUES ('j1','fetch','a1','pending',?)`, ts); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	// Apply the rest (0013).
	if _, err := Migrate(db); err != nil {
		t.Fatalf("apply 0013 (must succeed with a jobs row referencing an artifact): %v", err)
	}

	// Data survived the rebuild, including the 0005 sources columns.
	var title, connector, cursor string
	if err := db.QueryRow(`SELECT title FROM artifacts WHERE id = 'a1'`).Scan(&title); err != nil || title != "T" {
		t.Errorf("artifact title after rebuild = %q (err %v), want T", title, err)
	}
	if err := db.QueryRow(`SELECT connector, cursor FROM sources WHERE id = 's1'`).Scan(&connector, &cursor); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if connector != "rss" || cursor != "cur" {
		t.Errorf("source columns after rebuild: connector=%q cursor=%q, want rss/cur", connector, cursor)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM encounters WHERE id = 'e1' AND artifact_id = 'a1'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("encounter after rebuild = %d (err %v), want 1", n, err)
	}

	// Every synced table is CRR-shaped: no foreign keys, a NOT NULL primary key, and
	// no non-PK unique index. This guards the whole set without the extension, so a
	// future migration re-introducing any of these on any synced table is caught.
	syncedTables := []string{
		"sources", "artifacts", "encounters", "enrichments", "entities",
		"artifact_entities", "links", "topics", "artifact_topics",
		"profile_interests", "profile_history", "engagements", "review_schedule",
		"claims", "conflicts", "renders",
	}
	for _, tbl := range syncedTables {
		if n := fkCount(t, db, tbl); n != 0 {
			t.Errorf("%s has %d foreign key(s) after 0013, want 0 (cr-sqlite forbids checked FKs)", tbl, n)
		}
		if !pkNotNull(t, db, tbl) {
			t.Errorf("%s has a nullable primary key after 0013", tbl)
		}
		if hasUniqueIndex(t, db, tbl) {
			t.Errorf("%s has a non-PK UNIQUE index after 0013", tbl)
		}
	}
}

func fkCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n
}

func pkNotNull(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name, "notnull", pk FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var notnull, pk int
		if err := rows.Scan(&name, &notnull, &pk); err != nil {
			t.Fatal(err)
		}
		if pk > 0 && notnull == 0 {
			return false // a pk column that is nullable
		}
	}
	return true
}

// hasUniqueIndex reports whether table has a unique index OTHER than the primary
// key's auto-index (origin 'pk'). cr-sqlite allows the PK's unique index but forbids
// any other (origin 'u' for a UNIQUE constraint, 'c' for a CREATE UNIQUE INDEX).
func hasUniqueIndex(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT "unique", origin FROM pragma_index_list(?)`, table)
	if err != nil {
		t.Fatalf("index_list(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var uniq int
		var origin string
		if err := rows.Scan(&uniq, &origin); err != nil {
			t.Fatal(err)
		}
		if uniq == 1 && origin != "pk" {
			return true
		}
	}
	return false
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
