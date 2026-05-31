// Package store is the persistence layer: a SQLite-backed materialized
// projection of the CRDT knowledge base (design Section 6 / 6.0). It exposes
// typed repositories over the synced core tables.
package store

import (
	"database/sql"
	"time"
)

// Store groups the repositories over a single database handle.
type Store struct {
	db          *sql.DB
	Sources     *SourceRepo
	Artifacts   *ArtifactRepo
	Encounters  *EncounterRepo
	Enrichments *EnrichmentRepo
}

// New wires the repositories to db. db is expected to be already migrated.
func New(db *sql.DB) *Store {
	return &Store{
		db:          db,
		Sources:     &SourceRepo{db: db},
		Artifacts:   &ArtifactRepo{db: db},
		Encounters:  &EncounterRepo{db: db},
		Enrichments: &EnrichmentRepo{db: db},
	}
}

// now is the ISO-8601 UTC timestamp written to created_at/updated_at columns.
// These are audit timestamps, not logical merge clocks (see 6.0 readiness note).
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
