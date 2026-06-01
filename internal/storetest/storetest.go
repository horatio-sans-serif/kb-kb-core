// Package storetest provides a fresh, migrated store for tests in other
// packages (the store package's own tests use an in-package helper).
package storetest

import (
	"path/filepath"
	"testing"

	"kb/internal/store"
)

// New opens a fresh temp SQLite store, migrates it, and returns it. The
// underlying database is closed via t.Cleanup.
func New(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("storetest: open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("storetest: migrate: %v", err)
	}
	return store.New(db)
}
