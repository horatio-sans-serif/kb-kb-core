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

// Store groups the repositories. Each repo holds a write querier (q) and a read
// querier (rq): mutations and Tx use q (the single-writer pool); Get/List/Count
// use rq (a separate, multi-connection read pool over the same WAL file). Inside
// Tx, both are the transaction.
type Store struct {
	db          *sql.DB // write pool, for beginning transactions; nil in a tx-scoped Store
	Sources     *SourceRepo
	Artifacts   *ArtifactRepo
	Encounters  *EncounterRepo
	Enrichments *EnrichmentRepo
	Jobs        *JobRepo
	Renders     *RenderRepo
	Reviews     *ReviewRepo
	Graph       *GraphRepo
	Claims      *ClaimRepo
	Conflicts   *ConflictRepo
	Topics      *TopicRepo
	Sync        *SyncRepo
	Audit       *AuditRepo
}

// New wires the repositories to a single pool for both reads and writes (used in
// tests and single-connection setups). db is expected to be already migrated.
func New(db *sql.DB) *Store {
	return newStore(db, db, db)
}

// NewWithReadPool routes reads to a separate read-only pool while mutations and
// transactions use the single-writer pool. WAL lets the read pool run concurrent
// readers against the writer, so long (LLM-bound) enricher writes don't serialize
// reads behind them. write must be the single-writer pool (SetMaxOpenConns(1)).
func NewWithReadPool(write, read *sql.DB) *Store {
	return newStore(write, write, read)
}

func newStore(db *sql.DB, q, rq Querier) *Store {
	return &Store{
		db:          db,
		Sources:     &SourceRepo{q: q, rq: rq},
		Artifacts:   &ArtifactRepo{q: q, rq: rq},
		Encounters:  &EncounterRepo{q: q, rq: rq},
		Enrichments: &EnrichmentRepo{q: q, rq: rq},
		Jobs:        &JobRepo{q: q, rq: rq},
		Renders:     &RenderRepo{q: q, rq: rq},
		Reviews:     &ReviewRepo{q: q, rq: rq},
		Graph:       &GraphRepo{q: q, rq: rq},
		Claims:      &ClaimRepo{q: q, rq: rq},
		Conflicts:   &ConflictRepo{q: q, rq: rq},
		Topics:      &TopicRepo{q: q, rq: rq},
		Sync:        &SyncRepo{q: q, rq: rq},
		Audit:       &AuditRepo{q: q, rq: rq},
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
	if err = fn(newStore(nil, tx, tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// TimeFormat is the canonical wire format for every timestamp string the system
// mints. Unlike time.RFC3339Nano it never trims trailing zeros, so the fraction
// is always nine digits and every timestamp is the same width. That makes byte
// (lexicographic) order identical to chronological order — the property the job
// queue's FIFO claim (ORDER BY created_at, id) and the run_after/claimed_at
// comparisons depend on. RFC3339Nano breaks it: it drops the fraction entirely
// on a whole second, and '.' (0x2E) sorts before digits while 'Z' (0x5A) sorts
// after, so "…05Z" wrongly sorts after "…05.000001Z".
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime renders t in TimeFormat at UTC.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// Now is the current time as a TimeFormat string (UTC).
func Now() string { return FormatTime(time.Now()) }

// now is the ISO-8601 UTC timestamp written to created_at/updated_at columns.
// All repositories write it explicitly so a column never mixes this format with
// SQLite's space-separated datetime('now') default. These are audit timestamps,
// not logical merge clocks (cr-sqlite owns causal ordering; see 6.0).
func now() string {
	return Now()
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

// withTx runs fn with a write querier that is guaranteed transactional, so a repo
// method whose correctness needs several statements to be atomic (a find-then-insert
// dedup, a reconcile re-point) is safe however it is called. If q is already a
// *sql.Tx (the caller wrapped it in Store.Tx) that transaction is reused; if it is
// the *sql.DB write pool (an auto-commit call) an immediate transaction is opened
// and committed on success. _txlock=immediate makes BEGIN take the write lock, so
// concurrent writers serialize rather than race.
func withTx(ctx context.Context, q Querier, fn func(Querier) error) error {
	switch v := q.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit tx: %w", err)
		}
		return nil
	default:
		// Every querier in this package is the *sql.DB write pool or a *sql.Tx, so
		// this is unreachable. Fail loudly rather than run a find-then-insert
		// non-atomically (which would silently admit duplicates) if a new Querier
		// implementation is ever introduced.
		return fmt.Errorf("withTx: non-transactional querier %T", q)
	}
}
