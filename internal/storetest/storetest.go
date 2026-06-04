// Package storetest provides a fresh, migrated store for tests in other
// packages (the store package's own tests use an in-package helper).
package storetest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"kb/internal/store"
)

// New opens a fresh temp SQLite store, migrates it, and returns it. The
// underlying database is closed via t.Cleanup.
func New(t *testing.T) *store.Store {
	st, _ := NewWithDB(t)
	return st
}

// NewWithDB is New but also returns the raw *sql.DB, so a test can set up state the
// public store API deliberately prevents — e.g. inserting the cross-replica
// duplicate artifacts the reconcile sweep merges.
func NewWithDB(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("storetest: open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("storetest: migrate: %v", err)
	}
	return store.New(db), db
}
