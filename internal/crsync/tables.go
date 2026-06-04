// Package crsync is the CRDT peer-sync layer (design §6.0 / §8.16). The knowledge
// core replicates conflict-free across devices via cr-sqlite (which turns the
// SQLite tables themselves into conflict-free replicated relations); the job queue
// and derived indexes are local to each device and rebuilt, never synced.
//
// This file (the table classification) and the changeset operations (changes.go)
// are pure and always built. Only the driver registration that loads the native
// extension is behind the `crsqlite` build tag (driver_crsqlite.go vs.
// driver_stub.go), so the default build carries no native code and runs single-node.
//
// crsql_as_crr requires each synced table to have a NOT NULL primary key, NO
// non-primary-key unique index, NO checked foreign key, and a DEFAULT on every
// non-PK NOT NULL column. Migration 0013 rebuilt the synced tables to satisfy this
// (verified against the real extension by internal/crsync/ascrr_test.go); dedup that
// the dropped unique indexes used to enforce moved to application-level
// find-then-insert + the reconcile sweep (the design's same_as merge).
package crsync

// synced is the knowledge core: the tables whose rows are replicated across
// devices (design §6.0). Order is the dependency/topological order CRR conversion
// should follow.
var synced = []string{
	"sources",
	"artifacts",
	"encounters",
	"enrichments",
	"entities",
	"artifact_entities",
	"links",
	"topics",
	"artifact_topics",
	"profile_interests",
	"profile_history",
	"engagements",
	"review_schedule",
	"claims",
	"conflicts",
	"renders",
}

// local is the per-device set: the job queue, the migration ledger, the audit log,
// and the derived full-text index — rebuilt locally, never synced (§6.0).
var local = []string{
	"jobs",
	"audit_log",
	"schema_migrations",
	"artifacts_fts",
	"sync_state", // per-peer changeset watermarks (Stage 3b)
}

// SyncedTables returns the tables that replicate, in CRR-conversion order.
func SyncedTables() []string { return append([]string(nil), synced...) }

// LocalTables returns the device-local tables that never sync.
func LocalTables() []string { return append([]string(nil), local...) }

// IsSynced reports whether a table replicates. An unclassified table defaults to
// false (fail safe: a new table is not synced until deliberately classified).
func IsSynced(table string) bool {
	for _, s := range synced {
		if s == table {
			return true
		}
	}
	return false
}
