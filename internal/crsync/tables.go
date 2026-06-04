// Package crsync is the CRDT peer-sync layer (design §6.0 / §8.16). The knowledge
// core replicates conflict-free across devices via cr-sqlite (which turns the
// SQLite tables themselves into conflict-free replicated relations); the job queue
// and derived indexes are local to each device and rebuilt, never synced.
//
// This file (the table classification) is pure and always built. The replication
// engine — loading the cr-sqlite extension, converting tables to CRRs, exchanging
// changesets — is behind the `crsqlite` build tag (engine.go), so the default
// build and its dependency footprint are unchanged: a device without the extension
// runs exactly as before, single-node.
//
// Deciding-test result (the §8.16 / "Phase 10" decision): cr-sqlite loads via
// mattn/go-sqlite3 and replicates correctly, but crsql_as_crr requires each synced
// table to have a NOT NULL primary key, NO non-primary-key unique indexes, and
// unchecked foreign keys. The current schema's DB-enforced dedup (the
// canonical_url / content_hash / (kind,name) unique indexes) and checked FKs are
// therefore incompatible. Adopting cr-sqlite requires a schema migration that
// drops those unique indexes and moves dedup to application-level reconciliation
// (the design's same_as merge), and declares synced-table FKs unchecked — the
// prerequisite tracked before this engine is wired to the live store.
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
