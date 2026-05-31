// Package store is the persistence layer: a SQLite-backed materialized
// projection of the CRDT knowledge base (design Section 6 / 6.0). It exposes
// typed repositories over the synced core tables.
//
// NULL conventions: columns backing *string/*float64 fields store SQL NULL when
// the pointer is nil (canonical_url, content_hash, relevance_score, ...). Plain
// string fields backing nullable text columns store "" and tolerate NULL on read
// (title, body_markdown). JSON text columns store NULL when empty via
// nullIfEmpty (encounters.context) except config, which keeps its '{}' default.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by Get methods when no row matches, so callers depend
// on the store package rather than database/sql.
var ErrNotFound = errors.New("not found")

// Querier is the subset of *sql.DB and *sql.Tx the repositories use, so the same
// repository methods run either auto-committed or inside a transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store groups the repositories over a single querier (a *sql.DB, or a *sql.Tx
// when inside Tx).
type Store struct {
	db          *sql.DB // nil for a transaction-scoped Store
	Sources     *SourceRepo
	Artifacts   *ArtifactRepo
	Encounters  *EncounterRepo
	Enrichments *EnrichmentRepo
}

// New wires the repositories to db. db is expected to be already migrated.
func New(db *sql.DB) *Store {
	return newStore(db, db)
}

func newStore(db *sql.DB, q Querier) *Store {
	return &Store{
		db:          db,
		Sources:     &SourceRepo{q: q},
		Artifacts:   &ArtifactRepo{q: q},
		Encounters:  &EncounterRepo{q: q},
		Enrichments: &EnrichmentRepo{q: q},
	}
}

// Tx runs fn inside a single immediate transaction (the DSN sets
// _txlock=immediate, so BEGIN takes the write lock and concurrent writers block
// rather than race). Every repository call on the *Store passed to fn executes
// on that transaction; do not use the outer Store inside fn. On error or panic
// the transaction rolls back; otherwise it commits.
//
// Transactions do not nest: the inner Store has no *sql.DB, so calling Tx on it
// returns an error. This also avoids a self-deadlock against the single-writer
// connection pool (a nested Tx would wait for the connection the outer Tx holds).
func (s *Store) Tx(ctx context.Context, fn func(*Store) error) (err error) {
	if s.db == nil {
		return errors.New("store: nested transactions are not supported")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err = fn(newStore(nil, tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// now is the ISO-8601 UTC timestamp written to created_at/updated_at columns.
// All repositories write it explicitly so a column never mixes this format with
// SQLite's space-separated datetime('now') default. These are audit timestamps,
// not logical merge clocks (cr-sqlite owns causal ordering; see 6.0).
func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// nullIfEmpty maps "" to a SQL NULL so optional JSON/text columns stay NULL
// rather than storing an empty string.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }
