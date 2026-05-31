package main

import (
	"path/filepath"
	"testing"
)

func TestRun_FreshDB(t *testing.T) {
	db := filepath.Join(t.TempDir(), "kb.db")
	if err := run(db); err != nil {
		t.Fatalf("run on fresh db: %v", err)
	}
	// Re-run is idempotent (migrations already applied).
	if err := run(db); err != nil {
		t.Fatalf("re-run: %v", err)
	}
}
