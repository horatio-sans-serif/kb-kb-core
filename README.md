# KB Core

The unified knowledge-base daemon: captures information from many sources,
enriches it with local AI, stores it under one CRDT-replicated data model, and
makes it searchable, browsable, and self-surfacing.

- Design: `UNIFIED_SYSTEM_DESIGN_PLAN.md`
- Consolidation context: `CONSOLIDATION_REPORT.md`
- Build plan: `IMPLEMENTATION_PLAN.md` (current: Phase 0 — Spine)

## Build and test

    go build ./...
    go test ./...

Requires CGO (clang) for the SQLite driver. FTS5 search (Phase 3) will need the
driver built with `-tags sqlite_fts5`.

## Run

    go run ./cmd/kbd --db kb.db
