//go:build crsqlite

// The replication engine — gated behind the `crsqlite` build tag because it
// depends on the native cr-sqlite extension being loadable. A build without the
// tag (the default) has no cr-sqlite dependency and runs single-node; with the tag
// and the extension present, the knowledge core replicates.
package crsync

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// driverName is the registered database/sql driver that loads the cr-sqlite
// extension on each connection.
const driverName = "sqlite3_crsqlite"

// initEntrypoint is cr-sqlite's extension init symbol.
const initEntrypoint = "sqlite3_crsqlite_init"

var (
	registerOnce  sync.Once
	extensionPath string // captured by the connect hook; first Open wins
)

// Open opens a SQLite database whose every connection loads the cr-sqlite extension
// from extPath. The first call fixes the extension path for the process. Use
// SetMaxOpenConns(1) on the returned pool and call Finalize before Close (cr-sqlite
// requires per-connection finalization).
func Open(extPath, dsn string) (*sql.DB, error) {
	extensionPath = extPath
	registerOnce.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				return conn.LoadExtension(extensionPath, initEntrypoint)
			},
		})
	})
	return sql.Open(driverName, dsn)
}

// Available reports whether the cr-sqlite extension at extPath loads and responds —
// the runtime half of the deciding test, so the daemon can degrade to single-node
// cleanly when it does not.
func Available(extPath string) bool {
	db, err := Open(extPath, ":memory:")
	if err != nil {
		return false
	}
	defer db.Close()
	var v int64
	return db.QueryRow(`SELECT crsql_db_version()`).Scan(&v) == nil
}

// AsCRR converts the named tables into conflict-free replicated relations, in the
// given (dependency) order. Each table must already exist and satisfy cr-sqlite's
// constraints (NOT NULL PK, no extra unique indexes, unchecked FKs).
func AsCRR(ctx context.Context, db *sql.DB, tables []string) error {
	for _, t := range tables {
		if _, err := db.ExecContext(ctx, `SELECT crsql_as_crr(?)`, t); err != nil {
			return fmt.Errorf("crsync: as_crr %q: %w", t, err)
		}
	}
	return nil
}

// DBVersion returns this replica's logical clock — the high-water mark a peer
// tracks to pull only newer changes.
func DBVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v int64
	if err := db.QueryRowContext(ctx, `SELECT crsql_db_version()`).Scan(&v); err != nil {
		return 0, fmt.Errorf("crsync: db_version: %w", err)
	}
	return v, nil
}

// SiteID returns this replica's globally-unique site id.
func SiteID(ctx context.Context, db *sql.DB) ([]byte, error) {
	var id []byte
	if err := db.QueryRowContext(ctx, `SELECT crsql_site_id()`).Scan(&id); err != nil {
		return nil, fmt.Errorf("crsync: site_id: %w", err)
	}
	return id, nil
}

// Change is one row of the crsql_changes changeset — a single column write with
// its CRDT version metadata. Callers treat it as opaque and round-trip it through
// Changes -> transport -> Apply.
type Change struct {
	Table      string
	PK         []byte
	CID        string
	Val        []byte
	ColVersion int64
	DBVersion  int64
	SiteID     []byte
	CL         int64
	Seq        int64
}

// Changes returns every change with db_version greater than since (a peer's
// watermark), plus the largest db_version observed — what to send a peer to bring
// it up to date. Includes forwarded changes (from other sites) so changes
// propagate transitively through a gossip topology.
func Changes(ctx context.Context, db *sql.DB, since int64) ([]Change, int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT "table", pk, cid, val, col_version, db_version, site_id, cl, seq
		 FROM crsql_changes WHERE db_version > ? ORDER BY db_version, seq`, since)
	if err != nil {
		return nil, since, fmt.Errorf("crsync: read changes: %w", err)
	}
	defer rows.Close()

	out := []Change{}
	max := since
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Table, &c.PK, &c.CID, &c.Val, &c.ColVersion, &c.DBVersion, &c.SiteID, &c.CL, &c.Seq); err != nil {
			return nil, since, fmt.Errorf("crsync: scan change: %w", err)
		}
		if c.DBVersion > max {
			max = c.DBVersion
		}
		out = append(out, c)
	}
	return out, max, rows.Err()
}

// Apply merges a peer's changeset into the local database in one transaction. The
// CRDT merge (cr-sqlite's INSERT INTO crsql_changes) keeps the causally-newer value
// per column, so applying is idempotent and order-independent across peers.
func Apply(ctx context.Context, db *sql.DB, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("crsync: begin apply: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO crsql_changes ("table", pk, cid, val, col_version, db_version, site_id, cl, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("crsync: prepare apply: %w", err)
	}
	defer stmt.Close()
	for _, c := range changes {
		if _, err := stmt.ExecContext(ctx, c.Table, c.PK, c.CID, c.Val, c.ColVersion, c.DBVersion, c.SiteID, c.CL, c.Seq); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("crsync: apply change to %q: %w", c.Table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("crsync: commit apply: %w", err)
	}
	return nil
}

// Finalize releases cr-sqlite's per-connection resources. Call before closing a
// pool that loaded the extension.
func Finalize(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `SELECT crsql_finalize()`); err != nil {
		return fmt.Errorf("crsync: finalize: %w", err)
	}
	return nil
}
