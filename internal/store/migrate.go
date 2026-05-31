package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"kb/migrations"
)

// Open opens (creating if needed) the SQLite database at path with WAL journaling
// and foreign-key enforcement applied to every pooled connection.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	return db, nil
}

// Migrate applies every embedded migration whose version is greater than the
// highest already recorded in schema_migrations, in version order, and returns
// the number applied. It is idempotent: re-running applies nothing.
//
// Convention: each migration file is named NNNN_description.sql and records its
// own version with `INSERT INTO schema_migrations(version) VALUES (NNNN)`.
func Migrate(db *sql.DB) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return 0, fmt.Errorf("read current schema version: %w", err)
	}

	pending, err := loadMigrations()
	if err != nil {
		return 0, err
	}

	applied := 0
	for _, m := range pending {
		if m.version <= current {
			continue
		}
		body, err := migrations.FS.ReadFile(m.name)
		if err != nil {
			return applied, fmt.Errorf("read migration %s: %w", m.name, err)
		}
		// Not wrapped in a transaction: the migration sets PRAGMA journal_mode,
		// which cannot run inside a transaction.
		if _, err := db.Exec(string(body)); err != nil {
			return applied, fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		applied++
	}
	return applied, nil
}

type migration struct {
	version int
	name    string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := versionFromName(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// versionFromName parses "0001_init.sql" into 1.
func versionFromName(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("bad migration name %q (want NNNN_description.sql)", name)
	}
	v, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0, fmt.Errorf("bad migration version in %q: %w", name, err)
	}
	return v, nil
}
