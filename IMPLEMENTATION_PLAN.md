# KB Core — Implementation Plan: Phase 0 (Spine)

Implements Phase 0 of `UNIFIED_SYSTEM_DESIGN_PLAN.md` §12: the core daemon
skeleton everything else depends on. Subsequent phases get their own plans.
Delete this file when all stages are Complete.

## Phase 0 Tech Decisions

- **Language**: Go 1.23+. Single static binary (design §4.2; rememberer precedent).
- **Module path**: `kb` (local; switch to a remote path if/when published).
- **SQLite driver**: `github.com/mattn/go-sqlite3` (cgo). Chosen over pure-Go
  `modernc.org/sqlite` because it supports loadable extensions — required for
  `sqlite-vec` (Phase 2) and the `cr-sqlite` CRDT engine (Phase 10). cgo is
  accepted for the compute peer; revisit for mobile targets at Phase 10.
- **IDs**: `github.com/oklog/ulid/v2` — lexicographically sortable, mintable
  offline on any replica (design §6.0).
- **HTTP**: stdlib `net/http` with Go 1.22+ `ServeMux` method/pattern routing.
  No web framework (boring-solution principle).
- **Tests**: stdlib `testing`, table-driven. Each test opens a fresh temp-file
  DB and applies migrations; no shared state; deterministic.
- **Layout**:

  ```
  kb-core/
    cmd/kbd/main.go        daemon entrypoint
    internal/config/       config load + validation
    internal/store/        db open, migration runner, repositories
    internal/jobs/         queue + DAG runner
    internal/events/       in-process event bus + SSE fan-out
    internal/api/          HTTP handlers
    migrations/*.sql       schema (0001_init.sql exists, verified)
    go.mod
  ```

---

## Stage 1: Project scaffold + migration runner

**Goal**: A `kbd` binary that opens the SQLite database, applies pending
migrations idempotently, and exits 0. Schema from `migrations/0001_init.sql`
(already written and verified against sqlite3).

**Success Criteria**:

- `go build ./...` produces a `kbd` binary.
- Running `kbd --db <path>` on an empty path creates the DB with WAL +
  `foreign_keys=ON` and all Phase 0 tables; `schema_migrations` has version 1.
- Running it again is a no-op (version already applied; no error, no re-run).
- Migrations are embedded via `//go:embed migrations/*.sql` (binary is
  self-contained).

**Tests**:

- `TestMigrate_FreshDB`: apply to temp DB, assert all tables exist and
  `schema_migrations.version = 1`.
- `TestMigrate_Idempotent`: apply twice, assert second call applies 0
  migrations and returns nil.
- `TestMigrate_PragmasSet`: assert `journal_mode = wal` and
  `foreign_keys = 1` after open.

**Status**: Complete (reviewed; all findings fixed) — `store.Open` /
`store.Migrate` + embedded migrations (`kb/migrations`) + `cmd/kbd`. 4 tests
green, `go vet` and `gofmt` clean, `kbd --db <path>` round-trips (applied 1, then
0; missing `--db` exits 2; verified with a space-containing path).

FTS5 and vec0 indexes live in Phase 2–3 search setup (local-only rebuilt indexes
per §6.0; FTS5 needs `-tags sqlite_fts5`), keeping the Phase 0 build
dependency-free.

Code-review fixes applied:

- Each migration now runs in its own transaction with its version row, so a
  partial failure rolls back fully and stays re-runnable (was: non-atomic, could
  brick the DB). New test `TestMigrate_PartialFailureIsRecoverable` (via an
  injectable `fs.FS`) proves it.
- The runner is the single source of truth for recorded versions (parsed from
  the filename and inserted by the runner); migration files are pure schema and
  must not touch `schema_migrations`.
- Connection PRAGMAs set only in the DSN (removed from the migration file);
  `_txlock=immediate` added; DSN built with `net/url` so paths with spaces/`?`/`#`
  are escaped.
- `db.SetMaxOpenConns(1)` (single-writer WAL) — see Stage 3 read-pool note.
- Removed the `updated_at` trigger (CRDT-hostile + self-firing); `updated_at` is
  set by the repository layer (Stage 2).
- Dropped the redundant standalone `relevance_score` index (hot-decay column).
- `cmd/kbd` split into `run() error` so deferred cleanup runs and startup is
  testable.

---

## Stage 2: Storage layer (repositories + ULID)

**Goal**: Typed Go access to the synced core tables (`sources`, `artifacts`,
`encounters`, `enrichments`) with ULID generation and the artifact/encounter
merge primitive that the dedup keystone (§8.2) will build on.

**Success Criteria**:

- `store.New(db)` exposes repositories with `Create`/`Get`/`List` for the four
  tables; all IDs are ULIDs minted locally (any replica may create offline).
- Mutating methods set `updated_at` explicitly (no DB trigger — see Stage 1
  review fixes and §6.0 cr-sqlite readiness).
- `ArtifactRepo.UpsertByCanonical(url, ...)` returns the existing artifact (no
  duplicate row) when `canonical_url` matches, else creates one — the
  one-artifact-many-encounters invariant in code, not just schema.
- `EncounterRepo.Record(artifactID, sourceID, ...)` always inserts a row.
- Soft-delete sets `deleted_at`; default reads exclude tombstoned rows.

**Tests**:

- `TestArtifact_UpsertByCanonical_Dedups`: two upserts of the same canonical URL
  yield one artifact.
- `TestArtifact_SoftDelete_Hidden`: deleted artifact absent from `List`, present
  in `ListIncludingDeleted`.
- `TestEncounter_CountIsRelevanceSignal`: N records → `COUNT = N` for the
  artifact (feeds §8.9 `encounter_factor`).
- `TestULID_Sortable`: IDs created in order sort lexicographically.
- `TestSource_CreateGet`, `TestEnrichment_CreateGet`: round-trip CRUD.

**Status**: Complete (full review applied) — `store.New` + four repositories,
`NewID` (oklog/ulid/v2), `UpsertByCanonical` dedup keystone, soft-delete with
`List`/`ListIncludingDeleted`, repository-managed `updated_at` (no trigger).
18 tests green under `go test -race`; `go vet`/`gofmt`/`build` clean.

Full-review fixes applied:

- **Soft-delete + dedup (critical):** the partial unique index on `canonical_url`
  is scoped to live rows (`... AND deleted_at IS NULL`), and `UpsertByCanonical`
  is now a single atomic `INSERT … ON CONFLICT(canonical_url) WHERE … DO UPDATE
… RETURNING`. Re-seeing a tombstoned URL inserts a new artifact instead of
  hitting a UNIQUE error (`TestArtifact_UpsertAfterSoftDelete`), and concurrent
  upserts cannot duplicate (`TestArtifact_ConcurrentUpsert_NoDuplicates`, `-race`).
- **NULL reads (critical):** `title`/`body_markdown` scanned via `sql.NullString`
  so a NULL (e.g. from a replica merge) reads as "" instead of failing
  (`TestArtifact_NullTitleBodyRead`).
- **Transaction seam:** repos hold a `Querier` (satisfied by `*sql.DB` and
  `*sql.Tx`); `Store.Tx(ctx, fn)` runs them in one immediate transaction; nested
  Tx is disallowed (avoids the single-writer deadlock). Enables Stage 3/5 atomic
  composition (`TestStore_TxCommit`/`_TxRollback`/`_NestedTxDisallowed`).
- **Timestamps:** all repos write `created_at` via `now()` (uniform RFC3339);
  artifact listing orders by ULID `id`, not a string timestamp.
- **`ErrNotFound` sentinel** so callers don't import `database/sql`
  (`TestArtifact_GetNotFound`).
- `UpsertByCanonical` honors caller type/privacy/lifecycle/status/score on insert.
- `cmd/kbd` `run()` covered by `TestRun_FreshDB`.

---

## Stage 3: Job queue + DAG runner

**Goal**: A persistent job queue over the local-only `jobs` table that runs
pipeline stages respecting `depends_on`, with retry/backoff and crash-resume
(design Principle 4).

**Success Criteria**:

- `jobs.Enqueue(tx, type, artifactID, dependsOn, payload)` participates in the
  caller's transaction (transactional enqueue — Decision 4: write artifact +
  enqueue its enrichment atomically).
- A worker claims `pending` jobs whose dependencies are all `done` and whose
  `run_after` has passed; marks `running` → `done`/`failed`.
- Failed jobs retry with exponential backoff up to a cap, then stay `failed`
  with `last_error` populated.
- On restart, `running` jobs orphaned by a crash are reclaimed.
- Add a separate read-only connection pool so concurrent API reads (Stage 5+) do
  not serialize behind the single writer set in `Open` (Stage 1 review, Minor 8):
  one write connection (`SetMaxOpenConns(1)`) + a read pool over the same WAL
  file.

**Tests**:

- `TestQueue_TransactionalEnqueue`: rolled-back tx leaves no job row.
- `TestDAG_RespectsDependencies`: job B (depends on A) does not run until A is
  `done`.
- `TestQueue_RetryBackoff`: a handler that fails twice then succeeds ends `done`
  with `attempts = 3` and increasing `run_after` gaps.
- `TestQueue_ResumeAfterCrash`: a `running` job with no live worker is reclaimed
  and completed.

**Status**: Not Started

---

## Stage 4: Event bus + SSE

**Goal**: In-process publish/subscribe bus (design §9) plus an HTTP SSE endpoint
so local tools and the future web UI can subscribe to lifecycle events.

**Success Criteria**:

- `events.Bus` supports `Publish(event)` and `Subscribe() <-chan Event` with
  multiple concurrent subscribers; slow subscribers are dropped, not blocking.
- `GET /events` streams events as SSE; a client receives an event published
  after it connected.
- The Phase 0 event set is wired: `artifact.ingested` published when an
  artifact+encounter is written (Stage 2 hook).

**Tests**:

- `TestBus_FanOut`: two subscribers both receive a published event.
- `TestBus_SlowSubscriberDropped`: a non-draining subscriber does not block
  publishers; bus stays live.
- `TestSSE_DeliversAfterConnect`: HTTP test client connected to `/events`
  receives an event published post-connection.

**Status**: Not Started

---

## Stage 5: HTTP API skeleton + config + scheduler stub

**Goal**: The daemon serves an authenticated HTTP API with a health check and a
minimal ingest endpoint, loads config, and registers a scheduler tick (launchd
wiring is Phase 4; here it is an internal ticker).

**Success Criteria**:

- `config.Load` reads DB path, listen address, API token, default privacy tier,
  cloud-LLM enable flag; missing required values fail fast with a clear message
  (error-handling guideline).
- `GET /healthz` returns 200 with build info, no auth.
- `POST /ingest` (token-required) accepts `{url|body, source_id}`, writes an
  artifact + encounter via Stage 2, enqueues an `ingest` job via Stage 3, emits
  `artifact.ingested`, returns the artifact id. This exercises the full spine
  end-to-end (the walking skeleton).
- Requests without a valid token get 401; the token is never logged.

**Tests**:

- `TestConfig_MissingTokenFails`: load with no token → descriptive error.
- `TestIngest_EndToEnd`: POST creates artifact + encounter + job and the test's
  `/events` subscriber observes `artifact.ingested`.
- `TestAuth_RejectsMissingToken`: `/ingest` without token → 401; `/healthz`
  without token → 200.

**Status**: Not Started

---

## Definition of Done (Phase 0)

- All five stages Complete; `go test ./...` green; `go vet` clean.
- A single command starts `kbd`, and `POST /ingest` round-trips an artifact
  through write → enqueue → event, observable on `/events`.
- This is the spine for Phase 1 (ingestion/dedup), which extends Stage 2's
  upsert into the full canonicalization + fingerprint path (§8.2).
