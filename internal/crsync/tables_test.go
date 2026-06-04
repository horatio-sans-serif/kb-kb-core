package crsync

import "testing"

func TestClassification_DisjointAndCovers(t *testing.T) {
	synced := SyncedTables()
	local := LocalTables()
	if len(synced) == 0 || len(local) == 0 {
		t.Fatal("both classifications must be non-empty")
	}

	seen := map[string]bool{}
	for _, s := range synced {
		if seen[s] {
			t.Errorf("duplicate in synced: %q", s)
		}
		seen[s] = true
	}
	for _, l := range local {
		if seen[l] {
			t.Errorf("table %q is both synced and local", l)
		}
	}

	// The knowledge core syncs; the job queue and derived indexes do not (§6.0).
	mustContain(t, synced, "artifacts", "encounters", "enrichments", "entities", "links", "claims", "conflicts", "topics")
	mustContain(t, local, "jobs", "schema_migrations", "artifacts_fts")
}

func TestIsSynced(t *testing.T) {
	if !IsSynced("artifacts") {
		t.Error("artifacts should be synced")
	}
	if IsSynced("jobs") {
		t.Error("jobs is local-only, must not sync")
	}
	if IsSynced("unknown_table") {
		t.Error("an unclassified table defaults to not-synced (fail safe)")
	}
}

func mustContain(t *testing.T, set []string, want ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, s := range set {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("classification missing %q", w)
		}
	}
}
