package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"kb/migrations"
)

// Open opens (creating if needed) the SQLite database at path with WAL
// journaling, foreign-key enforcement, and immediate-locking transactions
// applied to every pooled connection via the DSN.
func Open(path string) (*sql.DB, error) {
	return OpenWithDriver(path, "sqlite3")
}

// OpenWithDriver is Open but using a named, already-registered database/sql driver
// instead of the default "sqlite3". The daemon uses it with the cr-sqlite driver
// (crsync.Register) so the write pool loads the replication extension on every
// connection while keeping the standard DSN pragmas and single-writer pool.
func OpenWithDriver(path, driver string) (*sql.DB, error) {
	db, err := sql.Open(driver, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// SQLite is a single-writer store; cap the pool to one connection to
	// serialize writers and avoid SQLITE_BUSY. A separate read-only pool over the
	// same WAL file is planned for Phase 2 Stage 0, when long LLM-bound write
	// transactions make concurrent reads worthwhile.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	// The database holds personal/sensitive-tier content; keep it owner-only so
	// other local accounts cannot read it directly, bypassing the token/tier
	// model. (The daemon also sets a 0077 umask so the WAL/SHM sidecars are
	// created owner-only.)
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure db file %q: %w", path, err)
	}
	return db, nil
}

// dsn builds a mattn/go-sqlite3 connection string, URL-escaping the path so
// paths containing spaces, '?', or '#' do not corrupt the query parameters.
func dsn(path string) string {
	q := url.Values{
		"_foreign_keys": {"on"},
		"_journal_mode": {"WAL"},
		"_busy_timeout": {"5000"},
		"_txlock":       {"immediate"},
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// OpenReadOnly opens a read-only, multi-connection pool over the same database
// file. WAL permits concurrent readers against the single writer, so reads do
// not serialize behind long write transactions. The file must already exist and
// be migrated (Open + Migrate run first on the write pool).
func OpenReadOnly(path string) (*sql.DB, error) {
	q := url.Values{"mode": {"ro"}, "_foreign_keys": {"on"}, "_busy_timeout": {"5000"}}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite (ro) %q: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite (ro) %q: %w", path, err)
	}
	return db, nil
}

// Migrate applies the embedded migrations. See migrateFS.
func Migrate(db *sql.DB) (int, error) {
	return migrateFS(db, migrations.FS)
}

// migrateFS applies every migration in src whose version exceeds the highest
// recorded in schema_migrations and returns the number applied.
//
// Each migration runs in its own transaction together with the row that records
// its version, so a migration that fails rolls back completely, leaving the
// database at the previous version and safely re-runnable. The runner is the
// single source of truth for recorded versions (parsed from the filename);
// migration files contain pure schema and must not touch schema_migrations.
//
// Migrations run on a single dedicated connection with foreign-key enforcement
// disabled. A table rebuild (e.g. 0013) DROPs a parent table that local-only tables
// (jobs, audit_log) still reference; with enforcement on — even deferred — the
// parent DROP raises a deferred FK violation that the subsequent re-create does not
// clear, so COMMIT fails although the final data is fully consistent. PRAGMA
// foreign_keys is a no-op inside a transaction, so it must be set on the connection
// before any migration tx begins; this is the standard SQLite practice for schema
// changes. Enforcement is restored before the connection returns to the pool, so
// normal operation keeps the jobs/audit_log -> artifacts foreign keys.
func migrateFS(db *sql.DB, src fs.FS) (int, error) {
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return 0, fmt.Errorf("read current schema version: %w", err)
	}

	all, err := loadMigrations(src)
	if err != nil {
		return 0, err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return 0, fmt.Errorf("migrate: disable foreign keys: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	applied := 0
	for _, m := range all {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, conn, src, m); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// applyMigration runs one migration body and records its version in a single
// transaction on conn. mattn/go-sqlite3 executes the multi-statement body
// sequentially on the transaction's connection; any failure rolls the whole
// migration back.
func applyMigration(ctx context.Context, conn *sql.Conn, src fs.FS, m migration) (err error) {
	body, err := fs.ReadFile(src, m.name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", m.name, err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", m.name, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("apply %s: %w", m.name, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (?)`, m.version); err != nil {
		return fmt.Errorf("record %s: %w", m.name, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", m.name, err)
	}
	return nil
}

type migration struct {
	version int
	name    string
}

func loadMigrations(src fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var all []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := versionFromName(e.Name())
		if err != nil {
			return nil, err
		}
		all = append(all, migration{version: v, name: e.Name()})
	}
	slices.SortFunc(all, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	return all, nil
}

// versionFromName parses the leading integer of "0001_init.sql" into 1.
func versionFromName(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("bad migration name %q (want <version>_<description>.sql)", name)
	}
	v, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0, fmt.Errorf("bad migration version in %q: %w", name, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("migration version in %q must be positive", name)
	}
	return v, nil
}
