// Package crsync is the multi-device replication layer (design §8.16 / §6.0). It
// turns the synced knowledge tables into cr-sqlite conflict-free replicated
// relations (CRRs) and exchanges their changesets between devices.
//
// The changeset operations here are plain SQL over *sql.DB: they compile in every
// build but only succeed at runtime when the database was opened with the cr-sqlite
// extension loaded (see Register, which is gated behind the `crsqlite` build tag so a
// default build cannot load native code). The daemon registers the driver and opens
// the write pool with it only when the extension is configured and loads; otherwise
// it runs single-node and never calls these.
package crsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnavailable means cr-sqlite is not available: the binary was built without the
// `crsqlite` tag, or no extension path was configured, or the extension failed to
// load. The daemon treats it as "run single-node".
var ErrUnavailable = errors.New("crsync: cr-sqlite not available")

// AsCRR converts the named tables into conflict-free replicated relations, in the
// given (dependency) order. Each table must already exist and satisfy cr-sqlite's
// constraints (NOT NULL PK, no extra unique indexes, no checked FKs, defaults on
// NOT NULL columns — see migration 0013). Idempotent: re-converting an existing CRR
// is a no-op, so it is safe to call on every startup.
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

// Change is one row of the crsql_changes changeset — a single column write with its
// CRDT version metadata. Callers treat it as opaque and round-trip it through
// Changes -> transport -> Apply. PK and SiteID are genuine blobs (cr-sqlite's packed
// primary key and the 16-byte site id); Val is a typed value (see Value).
type Change struct {
	Table      string `json:"table"`
	PK         []byte `json:"pk"`
	CID        string `json:"cid"`
	Val        Value  `json:"val"`
	ColVersion int64  `json:"col_version"`
	DBVersion  int64  `json:"db_version"`
	SiteID     []byte `json:"site_id"`
	CL         int64  `json:"cl"`
	Seq        int64  `json:"seq"`
}

// Value is one SQLite value from a changeset, preserving its storage class
// (null/integer/real/text/blob) across JSON. Without this, scanning val into []byte
// and re-applying it would store every value as a BLOB, so a TEXT like 'note' would
// become x'6e6f7465' and fail a type-sensitive CHECK or comparison. X is one of nil,
// int64, float64, string, or []byte (what database/sql yields for the storage class).
type Value struct{ X any }

type valueWire struct {
	T string          `json:"t"`
	V json.RawMessage `json:"v,omitempty"`
}

func (val Value) MarshalJSON() ([]byte, error) {
	w := valueWire{}
	switch x := val.X.(type) {
	case nil:
		w.T = "null"
	case int64:
		w.T, w.V = "int", mustJSON(x)
	case float64:
		w.T, w.V = "real", mustJSON(x)
	case string:
		w.T, w.V = "text", mustJSON(x)
	case []byte:
		w.T, w.V = "blob", mustJSON(x) // Go base64-encodes []byte
	default:
		return nil, fmt.Errorf("crsync: unsupported changeset value type %T", x)
	}
	return json.Marshal(w)
}

func (val *Value) UnmarshalJSON(data []byte) error {
	var w valueWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	switch w.T {
	case "null":
		val.X = nil
	case "int":
		var i int64
		if err := json.Unmarshal(w.V, &i); err != nil {
			return err
		}
		val.X = i
	case "real":
		var f float64
		if err := json.Unmarshal(w.V, &f); err != nil {
			return err
		}
		val.X = f
	case "text":
		var s string
		if err := json.Unmarshal(w.V, &s); err != nil {
			return err
		}
		val.X = s
	case "blob":
		var b []byte
		if err := json.Unmarshal(w.V, &b); err != nil {
			return err
		}
		val.X = b
	default:
		return fmt.Errorf("crsync: unknown changeset value type %q", w.T)
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// DefaultPageRows bounds one Changes page to this many rows (column-changes), so a
// fresh peer pulling from since=0 does not materialize the entire history at once.
// The puller loops pages until caught up. cr-sqlite's crsql_changes virtual table
// only supports a simple `db_version > ? ORDER BY db_version, seq LIMIT ?` scan (a
// computed-boundary subquery errors), so paging is row-based with the commit
// boundary handled in Go.
const DefaultPageRows = 10000

const changeCols = `"table", pk, cid, val, col_version, db_version, site_id, cl, seq`

// Changes returns one page of changes with db_version greater than since (a peer's
// watermark) plus the largest db_version returned. A page never splits a commit's
// column-changes (all rows of one db_version go together) so the watermark is always
// safe to advance: every change at or below the returned version was included, and
// the next pull (db_version > version) skips nothing. maxRows <= 0 uses
// DefaultPageRows. Includes forwarded changes (from other sites) so changes propagate
// transitively through a gossip topology.
func Changes(ctx context.Context, db *sql.DB, since int64, maxRows int) ([]Change, int64, error) {
	if maxRows <= 0 {
		maxRows = DefaultPageRows
	}
	// Fetch one extra row to detect truncation mid-commit.
	all, err := scanChanges(ctx, db,
		`SELECT `+changeCols+` FROM crsql_changes WHERE db_version > ? ORDER BY db_version, seq LIMIT ?`,
		since, maxRows+1)
	if err != nil {
		return nil, since, err
	}
	if len(all) <= maxRows {
		return all, maxVersion(all, since), nil // complete: this is the last page
	}
	// Truncated: the highest db_version present may be partial. Keep only the
	// db_versions strictly below it (those are fully included); the partial one is
	// re-fetched whole on the next page.
	lastDV := all[len(all)-1].DBVersion
	var keep []Change
	keepMax := since
	for _, c := range all {
		if c.DBVersion < lastDV {
			keep = append(keep, c)
			if c.DBVersion > keepMax {
				keepMax = c.DBVersion
			}
		}
	}
	if len(keep) > 0 {
		return keep, keepMax, nil
	}
	// A single commit larger than the page: fetch the whole commit so it is never
	// split (bounded by the commit's own size).
	commit, err := scanChanges(ctx, db,
		`SELECT `+changeCols+` FROM crsql_changes WHERE db_version > ? ORDER BY db_version, seq`, lastDV-1)
	if err != nil {
		return nil, since, err
	}
	var out []Change
	for _, c := range commit {
		if c.DBVersion != lastDV {
			break // only this commit's rows (ordered, so they come first)
		}
		out = append(out, c)
	}
	return out, lastDV, nil
}

// scanChanges runs a changeset query and decodes the rows, preserving each value's
// SQLite storage class (see Value).
func scanChanges(ctx context.Context, db *sql.DB, query string, args ...any) ([]Change, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("crsync: read changes: %w", err)
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		var val any
		if err := rows.Scan(&c.Table, &c.PK, &c.CID, &val, &c.ColVersion, &c.DBVersion, &c.SiteID, &c.CL, &c.Seq); err != nil {
			return nil, fmt.Errorf("crsync: scan change: %w", err)
		}
		c.Val = Value{X: val}
		out = append(out, c)
	}
	return out, rows.Err()
}

func maxVersion(changes []Change, since int64) int64 {
	m := since
	for _, c := range changes {
		if c.DBVersion > m {
			m = c.DBVersion
		}
	}
	return m
}

// Apply merges a changeset into the local database in one transaction. The CRDT
// merge (cr-sqlite's INSERT INTO crsql_changes) keeps the causally-newer value per
// column, so applying is idempotent and order-independent across peers. This is the
// low-level primitive and does not police table names; the production path
// (DBChangeStore.Apply) validates that every change targets a synced table before
// reaching here, because a peer's changeset is untrusted input.
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
		if _, err := stmt.ExecContext(ctx, c.Table, c.PK, c.CID, c.Val.X, c.ColVersion, c.DBVersion, c.SiteID, c.CL, c.Seq); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("crsync: apply change to %q: %w", c.Table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("crsync: commit apply: %w", err)
	}
	return nil
}

// Finalize releases cr-sqlite's per-connection resources. Call before closing a pool
// that loaded the extension.
func Finalize(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `SELECT crsql_finalize()`); err != nil {
		return fmt.Errorf("crsync: finalize: %w", err)
	}
	return nil
}
