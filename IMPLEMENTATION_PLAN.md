# KB Core — Implementation Plan

The living build plan, tracking the phases of `UNIFIED_SYSTEM_DESIGN_PLAN.md`
§12. Phase 0 (the spine) is broken into the five stages below; later phases are
appended as they are designed and built.

## Documentation & Walkthrough (ongoing, system-wide deliverable)

**Requirement:** a long-form walkthrough of every part of this system is needed
when it is done — what each subsystem does, how the pieces compose, and how a
request flows end to end.

**Approach — build it as we go, don't backfill at the end.** Each phase produces
one walkthrough document under `docs/walkthrough/NN-<name>.md` while the work is
fresh; together they accumulate into the long-form system walkthrough. Each entry
covers: what was built, why (linking the design section), how it fits the spine,
the request/data flow it participates in, and the key decisions/trade-offs.

**Reuse the doc/learning projects already in `~/projects/kb/`** rather than
building new surfaces:

- **Output/Render layer** (design "Output / Render Layer"): the walkthrough is
  Markdown-at-rest, so `pandoc` renders it to PDF/HTML and `patat` to a
  terminal slide-walkthrough — the render layer's first real consumer.
- **`agent-skills-tutorial`** (interactive HTML tutorial format): the shape for
  an interactive, click-through version of the walkthrough.
- **`glance`** (detects learning material; its "Copy Claude Instructions" button
  emits a tailored prompt to build demos + interactive tutorials): use that
  prompt pattern to generate the interactive walkthrough from each phase doc.
- **`flashcarder`** (spaced-repetition flashcards from content): generate
  concept-review cards from each walkthrough doc so the system's own design is
  learnable/retained.

Status: structure established (`docs/walkthrough/README.md`); per-phase entries
written as each phase completes.

## Phase 0 Tech Decisions

- **Language**: Go 1.24 (`go.mod`). Single static binary (design §4.2; rememberer precedent).
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
- `cmd/kbd` split into a testable `serve(...)` so deferred cleanup runs and startup is
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
- `cmd/kbd` `serve()` covered by `TestServe_HealthzThenGracefulShutdown`.

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

**Status**: Complete — `store.JobRepo` (Enqueue / Claim / MarkDone / Reschedule /
MarkFailed / ReclaimStuck / Get) + `jobs.Runner` (handlers, `RunOnce`, `Drain`,
`ExpBackoff`, `ReclaimStuck`). 10 new tests green under `go test -race`;
`go vet`/`gofmt`/`build` clean.

- Transactional enqueue via `Store.Tx` + `Jobs.Enqueue`
  (`TestJobs_TransactionalEnqueue`).
- Dependency gating in a single race-free `UPDATE … WHERE id = (SELECT … WHERE
NOT EXISTS json_each(depends_on) …) RETURNING` claim
  (`TestJobs_ClaimBlockedByDependency`, `TestRunner_DAGOrder`).
- Retry/backoff with attempt cap then permanent fail (`TestRunner_RetryThenSucceed`,
  `TestRunner_ExhaustsRetriesThenFails`); backoff curve tested separately
  (`TestBackoff_Increasing`). Unknown job type fails fast
  (`TestRunner_UnknownTypeFails`).
- Crash-resume via `ReclaimStuck` (`TestJobs_ReclaimStuck`,
  `TestRunner_ResumeAfterCrash`).

Deferred: the separate read-only connection pool moves to Stage 5, where the
HTTP API first introduces concurrent reads; the Stage 3 runner drains
single-threaded on the one writer, so it is not needed yet.

Review fixes applied (24 tests total under `-race`):

- **Dependency stranding (major):** `MarkFailed` now cascade-fails pending
  dependents to a fixpoint, so a broken stage doesn't silently freeze its
  downstream subtree (`TestJobs_CascadeFailOnDependencyFailure`,
  `TestRunner_CascadeFailsDependents`). `Blocked()` surfaces jobs stranded on a
  missing dependency id (`TestJobs_Blocked`) for Stage 4 to emit as events.
- **Claim lease (major):** added a `claimed_at` column, stamped on claim;
  `ReclaimStuck(olderThan)` reclaims only leases at/before a cutoff
  (`TestJobs_ReclaimByLease`). Startup still reclaims all; a `TODO(Stage 5)` marks
  the switch to a lease cutoff for the continuous runner.
- **Drain resilience (major):** a per-job infra error is recorded (joined) and
  draining continues; only a claim error (likely persistent) stops it. Handler
  errors are wrapped into the infra error so they're never lost.
- **Status-guarded transitions (minor):** `MarkDone`/`Reschedule`/`MarkFailed` are
  scoped to `status='running'`, so a terminal job can't be resurrected by a
  lost-claim race.
- Single shared timestamp formatter (`tsFormat`); `ExpBackoff` guards `base<=0`
  and `max<base`; `Handle` panics on duplicate registration.
- Coverage: concurrent claim has no double-dispatch over an 8-connection pool
  (`TestJobs_ConcurrentClaim_NoDoubleDispatch`, `-race`); `maxAttempts=1` runs a
  handler exactly once (`TestRunner_MaxAttemptsOne_RunsExactlyOnce`).

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

**Status**: Complete — `internal/events` (`Bus` with non-blocking fan-out,
buffered per-subscriber channels, idempotent unsubscribe; `Event` + the
Section 9 type constants) and `internal/api.SSEHandler` (subscribes before
flushing headers, so a connected client misses nothing; ends on client
disconnect). 4 new tests green under `go test -race`; `go vet`/`gofmt`/`build`
clean.

- Fan-out to multiple subscribers (`TestBus_FanOut`); a full slow subscriber
  drops events without blocking publishers (`TestBus_SlowSubscriberDropped`);
  unsubscribe closes the channel and is safe (`TestBus_UnsubscribeStopsDelivery`).
- SSE stream delivers post-connect events (`TestSSE_DeliversAfterConnect`).

Deferred to Stage 5: publishing `artifact.ingested` happens at the ingest point,
which is the Stage 5 `POST /ingest` handler (write artifact + encounter +
enqueue + publish, atomically via `Store.Tx`). The bus and stream this stage
provides are what that handler will publish to. Keeping the publish out of the
`store` package preserves layering (store has no dependency on `events`).

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

**Status**: Complete — `internal/config` (env-driven, fails fast naming missing
vars, validates privacy tier), `internal/api` (`NewServer`/`Handler`: public
`GET /healthz`, auth-wrapped `GET /events` SSE and `POST /ingest`; constant-time
bearer-token check, token never logged), `jobs.Runner.Run` (reclaim-once then
tick-drain), and `cmd/kbd` (config → open/migrate → runner loop → HTTP serve,
graceful shutdown on SIGINT/SIGTERM). 14 new tests green under `go test -race`.

- `POST /ingest` is the walking skeleton: writes artifact + encounter + enqueues
  an `ingest` job atomically via `Store.Tx`, then publishes `artifact.ingested`
  only after commit (`TestIngest_EndToEnd`). Unknown `source_id` → 400
  (`TestIngest_UnknownSource`).
- Config fails fast (`TestConfig_MissingTokenFails`/`_MissingDBFails`/
  `_InvalidPrivacyFails`); auth rejects missing/wrong token while `/healthz`
  stays open (`TestAuth_RejectsMissingToken`).
- Real-binary smoke verified over a socket: `/healthz` 200, `/ingest` 401/400 as
  expected, graceful shutdown on signal.

Deferred (with consumers, not Phase 0): the read-only connection pool moves to
the retrieval phase (no concurrent DB reader exists yet — `/healthz` is
DB-free, the runner and `/ingest` are the only writers, both serialized on the
one writer; SSE holds no DB connection). The `ingest` job handler is a
placeholder until Phase 2 wires the enrichment pipeline.

Review fixes applied (54 tests total under `-race`):

- **HTTP timeouts (critical):** `ReadHeaderTimeout` 5s / `ReadTimeout` 15s /
  `IdleTimeout` 120s; `WriteTimeout` deliberately unset (a server-wide write
  deadline would sever the long-lived `/events` SSE stream), documented inline.
- **Body limit (critical):** `/ingest` wraps the body in `http.MaxBytesReader`
  (8 MiB) → 413 (`TestIngest_BodyTooLarge`, verified over a real socket).
- **Runner lifecycle:** `ReclaimStuck` now runs synchronously at startup (a
  failure is fatal, not a silently dead loop); `Run` is the drain loop only and
  logs non-context drain errors; the runner goroutine is joined at shutdown.
- **Defense-in-depth:** an empty configured token never authenticates
  (`TestAuth_EmptyConfiguredTokenRejects`).
- **Coverage backfill:** `body`/note ingest, malformed JSON → 400, missing-field
  → 400, and API-level re-ingest dedup (`TestIngest_BodyType`,
  `_MalformedJSON`, `_MissingFields`, `_ReingestDedups`).
- **Forward constraints documented:** `/events` header-auth vs browser
  `EventSource` (Stage 10), `KB_LISTEN_ADDR` beyond-loopback exposure, and a
  `TODO(Phase 2)` for the redundant enrichment enqueue on re-ingest.

---

## Definition of Done (Phase 0) — MET

- All five stages Complete; `go test -race ./...` green (48 tests); `go vet` and
  `gofmt` clean.
- A single command (`kbd`) starts the daemon, and `POST /ingest` round-trips an
  artifact through write → enqueue → event, observable on `/events`.
- This is the spine for Phase 1 (ingestion/dedup), which extends Stage 2's
  upsert into the full canonicalization + fingerprint path (§8.2).

---

# Phase 1 — Ingestion: canonicalization + content-hash dedup (§8.2)

**Goal:** make ingestion _correct_ — the same artifact arriving from different
sources or at differently-decorated URLs collapses to one canonical artifact.

## Stage 1.1: URL canonicalization

**Goal**: a pure `canon.URL` that normalizes a raw URL so tracking-decorated and
trivially-different URLs map to one canonical form.

**Success Criteria**:

- Strips tracking params (`utm_*`, `fbclid`, `gclid`, `mc_eid`, `igshid`,
  `ref`/`ref_src`, ...); lowercases scheme + host; drops default ports; removes
  `www.`; strips the fragment; removes a trailing slash (except root); sorts the
  remaining query for stable output; conservative de-AMP.
- Pure and offline. Redirect-following is deferred to fetch time (Phase 2), which
  re-canonicalizes the resolved URL.
- Wired into `POST /ingest`: the stored `canonical_url` is the canonicalized
  form; the original is preserved as the encounter's `raw_url`.

**Tests**: table-driven `canon.URL` cases (each normalization + idempotence:
`URL(URL(x)) == URL(x)`); an API test that two URLs differing only by `utm_*`
dedup to one artifact with two encounters.

**Status**: Complete — `internal/canon.URL` (lowercase scheme/host, drop
userinfo/default-port/`www.`/fragment, strip `utm_*` + tracking denylist, sort
query, trailing-slash + conservative de-AMP); wired into `/ingest` (canonicalize
→ 400 on bad URL → `CanonicalURL`, original preserved as the encounter `raw_url`).
`TestURL`/`TestURL_Invalid`; `TestIngest_CanonicalDedup` (utm-only-difference →
one artifact, two encounters) and `TestIngest_InvalidURL` (400).

## Stage 1.2: Content fingerprint + content-hash dedup

**Goal**: dedup the same content living at different URLs / provided as a body.

**Success Criteria**:

- `canon.Content` normalizes text (collapse whitespace, trim) and returns a
  stable hash.
- A partial unique index on `content_hash` (live rows) + `UpsertByContentHash`
  (atomic `ON CONFLICT … RETURNING`), mirroring `UpsertByCanonical` (migration
  `0002`).
- `POST /ingest` of a `body` dedups by content hash; URL artifacts carry a NULL
  content hash until fetched (Phase 2), so the two dedup paths don't collide.

**Tests**: `canon.Content` normalization + stability; `UpsertByContentHash`
dedups; an API test that two identical bodies collapse to one artifact.

**Status**: Complete — `internal/canon.Content` (whitespace-collapsed SHA-256);
migration `0002` swaps the non-unique content*hash index for a partial unique one
(live rows) — the first multi-migration, exercising the runner; `store.upsert`
refactored to share insert/conflict between `UpsertByCanonical` and the new
`UpsertByContentHash`. `/ingest` of a `body` dedups by content hash (URL artifacts
keep a NULL hash until fetched, so paths don't collide).
`TestContent*\*`, `TestArtifact_UpsertByContentHash_Dedups`,
`TestIngest_ContentDedup`. Migrate tests made count-robust for multi-migration.
61 tests total green under `-race`; index swap verified at the SQL level.

---

# Phase 2 — Stage 0: Foundations (from the comprehensive review)

The full-system review surfaced a cluster of architectural prerequisites: Phase 0
built the mechanisms (queue, store, bus) but not the contracts that the
enrichment pipeline plugs into. Do these **before** the first real enricher, in
roughly this order — each is small now and a refactor later.

**Status: Complete.** 0.1 `internal/kb` domain package (type aliases in `store`,
typed enums). 0.2 `internal/pipeline` (DAG expansion + privacy-gated handlers).
0.3 `internal/ssrf` guard. 0.4 read pool (`OpenReadOnly`/`NewWithReadPool`). 0.5
`SourceRepo` List/Update/SetEnabled + `internal/sources` connector registry. 0.6
token-length floor + ingest UTF-8/control-char validation. Deferred to Phase 4
(needs real connectors): wiring connector-registry validation at source create
and startup. Deferred to the read API: audit-log writes + per-device tokens (no
read/export path to audit yet). All green under `-race`.

## 0.1 Domain types package (`internal/kb`)

Extract `Artifact`, `Enrichment`, `Encounter`, `Source` (and value types
`PrivacyTier`, `SourceKind`, `ArtifactType`, `LifecycleTier`, `Status`) into a
pure `internal/kb` package with no `database/sql` dependency. `store` maps
domain types to/from rows. `jobs`, `api`, and the future `fetch`/`enrich`/`render`
packages depend on `internal/kb`, not on `store` — so plugins don't import the
SQL layer (the coupling design §7 forbids). Typed enums replace the `string`
fields; storage-shaped fields (string timestamps, `*string` tombstones) stay in
`store`'s row structs. **Why now:** a 5-file rename today vs. a codebase-wide one
after three plugin packages exist.

## 0.2 Pipeline / stage abstraction + `Enricher` contract

Introduce a pipeline package that owns the `Enricher` interface (`Name`,
`DependsOn() []string`, `PrivacyOK(PrivacyTier) bool`, `Enrich`), a registry, and
a topological expansion that — given an artifact — enqueues one job per stage with
`depends_on` wired from `DependsOn()`. Keep `jobs.Runner` the dumb executor it
is; add the DAG/stage semantics above it. The placeholder `ingest` handler in
`cmd/kbd` becomes this expansion. Declare the `Source`/`Fetcher`/`Renderer`
interfaces in `internal/kb` now (even unimplemented) so the seam and its types
(`ResourceRef`, `Extracted`, `Cursor`, `Health`, `RenderOpts`) are fixed.

## 0.3 Privacy enforcement + SSRF guard (security-critical for Phase 2)

- **Privacy gate:** make `PrivacyOK` a hard precondition in the enricher dispatch
  path — a `sensitive`/`restricted` artifact is never dispatched to a cloud
  enricher regardless of `KB_CLOUD_LLM`; the flag only _enables_ cloud for
  `public`/`personal`. Test that a sensitive artifact never reaches a cloud
  handler. (Per-source tagging is already done; this enforces it.)
- **SSRF guard:** a dedicated, unit-tested package the fetcher _must_ call —
  scheme allowlist (`http`/`https` only), resolve-then-pin the IP and reject
  loopback / private / link-local / ULA / multicast / Tailscale CGNAT
  (`100.64.0.0/10`) on every redirect hop, cap redirects and response size, set
  per-fetch timeouts. The fetcher runs on a host next to localhost sidecars, so
  this is required before any URL is fetched.

## 0.4 Read pool

Add a second, read-only `*sql.DB` (N connections) over the same WAL file; keep
the writer at `SetMaxOpenConns(1)`. Route `Get`/`List` to the read pool,
mutations and `Tx` to the writer. WAL already allows concurrent readers against
the writer; this stops enricher writes (long, LLM-bound) and `/ingest` from
serializing reads behind them.

## 0.5 Source surface + connector registry

`SourceRepo` needs `List`/`Update`/`SoftDelete` (the scheduler §8.1 and health
monitor §8.15 enumerate and mutate sources). Add a `connector -> Source`
registry and validate at source-create and at startup that every enabled
source's connector is registered. Map the deferred `schedule`/`auth_state`/
`storage_budget` columns.

## 0.6 Audit + identity hardening

- Write `audit_log` entries on ingest of (and future reads of) `sensitive`/
  `restricted` artifacts (actor, action, artifact_id, tier).
- Move from one static `KB_API_TOKEN` to **per-device tokens** (a credentials
  table or signed tokens) so each device is individually revocable and populates
  `audit_log.actor`; add a token-entropy floor in `config.Load`.
- Reject invalid UTF-8 / control characters in ingest text (stored-injection
  hardening for the Phase 10 renderers); output-encode at render time.

## Already realized (Phase 1.x review)

Event-driven runner + claim index + job purge; cascade-fail as a recursive CTE;
per-source privacy tiering + tier validation; DB file `0600` + `0077` umask; slog;
SSE write-error + heartbeat; bus RWMutex; canon allocation/typed-event cleanups;
DB-file and HTTP hardening; tooling (Makefile, golangci-lint, CI); the
notification-vs-durable-work bus rule (design §9). Coverage backfilled across all
packages.

---

# Phase 2 — Stage 1: Fetch layer (the enrichment pipeline goes live)

**Goal:** raw URL artifacts become artifacts with real content — the first end of
the design's enrichment core (§8.3), wired through the pipeline into ingest.

**Status: Complete (reviewed; all findings fixed).**

- **Contract evolution.** `kb.Enricher.Enrich` now returns
  `EnrichResult{Enrichments, Patch}` — a stage can emit enrichment rows _and_
  patch the artifact (fetch sets the body). `store.ArtifactRepo.Patch` applies a
  narrow per-column UPDATE.
- **Status lifecycle.** `pipeline.EnqueueTx` marks the artifact `enriching`; the
  runner's on-complete hook (`pipeline.finalize`) sets the terminal status by
  reading the job table after each completion — `ready` when every stage is done,
  `failed` when any stage failed — and publishes `artifact.ready`/`.failed`.
- **Fetch.** `internal/fetch.WebFetcher` — SSRF-guarded http.Client (dialer
  Control rejects internal IPs, defeating rebinding; redirect/size/timeout caps)
  plus a pre-flight `CheckURL` host check; rejects non-HTML content types and
  oversized bodies; strips `script`/`style`/`noscript`; basic HTML→Markdown.
  `internal/enrich.Fetch` is the "fetch" stage.
- **Wiring.** `api.Deps.Enqueue` (an `Enqueuer` seam) lets `/ingest` expand the
  artifact into its pipeline jobs in the ingest transaction — only on a fresh
  insert, so re-ingesting a known URL records the encounter but does not re-run
  enrichment; `cmd/kbd` registers the fetch stage. Verified end to end: HTTP
  ingest → fetch → body in the artifact → `ready`.

**Review fixes applied (Phase 2 Stage 1):**

- **Failed stage no longer strands the artifact.** The former terminal `@ready`
  job depended on every stage, so an upstream failure cascade-failed it and the
  artifact sat in `enriching` forever. Replaced with `jobs.Runner.SetOnComplete`
  - `pipeline.finalize`, which derives terminal status from
    `store.JobRepo.ArtifactPipelineState` (pending count + any-failed). A failing
    stage now finalizes the artifact `failed`.
- **Privacy-gate skips are auditable.** A gated stage writes a `skipped`
  enrichment row instead of silently returning, and still finalizes (a skip is
  not a failure).
- **`Patch` cannot resurrect a tombstone.** Scoped to `deleted_at IS NULL`.
- **One sortable timestamp format.** `store.TimeFormat`/`FormatTime`/`Now` replace
  `time.RFC3339Nano` everywhere a timestamp is minted (store, jobs, daemon).
  RFC3339Nano trims trailing zeros and drops the fraction on a whole second, which
  broke the lexicographic = chronological invariant the FIFO `Claim` and
  `run_after`/`claimed_at` comparisons rely on. Fixed-width nanoseconds restore it.

## Phase 2 — Stage 2: Structured LLM enrichment (summarize)

**Goal:** every text artifact gains a summary, topical tags, and the mandatory
critical analysis from one schema-validated local-LLM call (design §8.5, the
`SUMMARIZE` stage at line 220; §6.4 makes critical analysis mandatory to prevent
uncritical hoarding).

**Status: Complete.** Verified end to end (HTTP ingest → fetch → summarize →
`ready` with `summary`/`tags`/`critical_analysis` rows) against a fake LLM; the
daemon wires a real local Ollama.

- **`internal/llm`.** A `Client` interface (`Generate`) over a local Ollama
  server (`/api/generate`, `stream:false`). The HTTP client is injectable so
  enrichers and the client itself test against an httptest fake; no network in
  tests. Supports Ollama structured output by passing a JSON schema in `format`.
- **`summarize` enricher.** `DependsOn: ["fetch"]` (runs after the body exists,
  for both fetched URLs and notes). One `Generate` call returns
  `{summary, tags[], critical_analysis}`; the stage writes three enrichment rows
  (`summary`, `tags`, `critical_analysis`). Tags are normalized (lowercase, trim,
  dedup, length-bounded, count-capped). The body is truncated to a byte cap on a
  rune boundary before the call.
- **Privacy is enforced, not assumed.** `PrivacyOK` is true for every tier only
  when the LLM endpoint is on-device. Config verifies `KB_OLLAMA_URL` is loopback;
  a remote endpoint requires `KB_ALLOW_REMOTE_OLLAMA=true`, and when opted in the
  stage degrades to the cloud floor (sensitive/restricted artifacts are gated off
  the remote model — §8.17). So the local-LLM privacy guarantee holds by
  construction.
- **Config + wiring.** `KB_OLLAMA_URL` (default `http://127.0.0.1:11434`),
  `KB_SUMMARIZE_MODEL` (default `llama3.2`), `KB_ALLOW_REMOTE_OLLAMA`. `cmd/kbd`
  registers `summarize` after `fetch` and warns when the endpoint is not loopback.

**Review fixes applied (Phase 2 Stage 2):** rune-safe body truncation (a byte cap
could split a UTF-8 rune); enforceable on-device privacy (above); assert Ollama's
`done` flag (a partial 200 body is not a complete answer); bound individual tag
length.

**~~Known limitation — at-least-once enrichment writes.~~ Resolved (Phase 3
Stage 5).** The pipeline handler now folds the job's `MarkDone` into the same
short transaction as its enrichment/patch writes, after the slow Enrich work
completes outside any transaction. Effects and completion are atomic: a crash
before commit re-runs the stage cleanly (nothing written); a commit lands the
rows with the `done` status, and a `done` job is never reclaimed — so a re-run
can't duplicate writes. Exactly-once.

## Phase 2 — Stage 3.1: Artifact embeddings

**Goal:** each text artifact gets a vector embedding from the local embed model,
stored as an `enrichment` row so it syncs and every replica can rebuild its local
ANN index (design §6.5 / §6.0; Decision 2 — local-only embeddings).

**Status: Complete.** Verified end to end (fetch → summarize + embed → `ready`
with an `embedding` row) against a fake embedder.

- **`internal/llm`.** An `Embedder` interface (`Embed`) over Ollama's `/api/embed`
  (`{model, input}` → `{embeddings: [[...]]}`), separate from `Client` so callers
  depend only on what they use. `*Ollama` implements both. Returns `[]float32`
  (matches the `vec0` `FLOAT[N]` storage type).
- **`embed` enricher.** `DependsOn: ["fetch"]` (needs the body; runs in parallel
  with `summarize`). Embeds the truncated body and writes one `embedding`
  enrichment row whose payload carries the model, dimension, and vector. No
  artifact patch. Same locality-aware `PrivacyOK` as `summarize` (the embed model
  is local Ollama; a remote endpoint gates sensitive/restricted off). Empty body
  is a graceful no-op.
- **Config + wiring.** `KB_EMBED_MODEL` (default `nomic-embed-text`). `cmd/kbd`
  registers `embed` after `fetch`.

## Phase 2 — Stage 3.2: Relevance scoring

**Goal:** score each artifact against the profile and write the score to the
artifact (design §8.9): `relevance = profile_match * recency * encounter *
engagement * source_weight`.

**Status: Complete.** Verified end to end (the full pipeline patches a positive
`relevance_score` and writes a `relevance` breakdown row).

- **`internal/relevance` (pure math).** `Cosine`, a weighted-interest `Profile`
  with `Match` (max weight-scaled cosine, clamped to [0,1]; empty profile is
  neutral = 1), the factor functions (`RecencyFactor` 30-day half-life,
  `EncounterFactor` log-growth, `EngagementFactor`), and `Score` combining them.
  `BuildProfile` embeds interest phrases (injected embed func — no I/O in the
  package). I/O-free and fully unit-tested.
- **`enrich.Relevance`.** `DependsOn: ["embed"]`. Reads the artifact's embedding,
  encounter count, and max source weight through an injected `SignalProvider`
  (the enricher stays store-free), scores it, and returns a
  `Patch{RelevanceScore}` plus a `relevance` enrichment row carrying the factor
  breakdown for explainability. No embedding yet → graceful no-op. `PrivacyOK` is
  always true (pure local computation over stored data).
- **`internal/relsignals`.** Store-backed `SignalProvider`: pulls the latest
  `embedding` enrichment (parsed via `kb.EmbeddingPayload`), encounter count, and
  `MAX(source_weight)` join. A missing embedding yields an empty vector, not an
  error.
- **Store.** `EnrichmentRepo.LatestByKind` and
  `EncounterRepo.MaxSourceWeightForArtifact` (defaults to 1.0 for no encounters).
- **Config + wiring.** `KB_INTERESTS` (semicolon-separated). `cmd/kbd` builds the
  profile at startup (best-effort: a neutral profile if the embed model is
  unreachable) and registers `relevance` after `embed`.

The enrichment DAG is now `fetch → {summarize, embed}; embed → relevance`.

**Review fixes applied (Phase 2 Stage 3):**

- **Non-finite embeddings fail loudly.** A NaN/±Inf vector component would make
  `json.Marshal` fail; the embed stage now rejects it as a stage error (visible,
  retryable) and marshals the payload explicitly, instead of the old lossy helper
  that wrote `{}` — which read back as "no embedding" and permanently unscored the
  artifact with no error.
- **Dimension mismatch surfaces.** If the profile and an artifact embedding differ
  in dimension (e.g. the embed model changed), the relevance stage errors rather
  than silently scoring 0 for everything.
- **`SourceWeight` floored at 0** in `Score` so a future yield feedback loop
  driving the column negative can't invert the `relevance_score DESC` ranking.
- **Honest factor docs.** `EncounterFactor`/`Score` now state plainly that, with
  scoring computed once at first ingest (count 1, single source), the multi-source
  encounter and source-weight factors are dormant until the re-scoring trigger
  (deferred to §9) runs relevance again on a re-encounter.

**Known limitation — scoring is computed once at ingest.** The relevance stage
runs in the first-ingest pipeline only, when the encounter count is 1 and one
source is known, and nothing recomputes it afterward — so the recency decay and
the multi-source factors don't yet differentiate artifacts over time. The §9
living-profile stage adds the re-scoring trigger (on re-encounter, engagement, and
profile change).

## Phase 3 — Stage 1: Semantic search

**Goal:** find artifacts by meaning, not keywords (design §8.8) — embed the query
and rank artifacts by cosine similarity to their stored embeddings, exposed at
`GET /search`.

**Status: Complete.** Verified end to end (ingest → embed → `GET /search` returns
the artifact) plus unit coverage of ranking, dim-skip, and the endpoint.

- **Brute-force first, `vec0` later.** Embeddings already live as `enrichment`
  rows (§6.0). Stage 1 ranks them in-process with `relevance.Cosine` and a top-K
  heap — no new dependency, correct results, fine at personal scale. The
  `sqlite-vec` (`vec0`) ANN index is the same ranking with sublinear candidate
  retrieval; it's a localized swap behind the same `Searcher` interface and is
  deferred until scale needs it. The O(n) per-query scan is the documented
  trade-off.
- **`internal/search`.** A `Searcher` embeds the query (injected `llm.Embedder`),
  streams the latest embedding per live artifact from the store, ranks by cosine,
  and returns the top-K `(artifact_id, score)`. Dimension-mismatched vectors
  (stale embed model) are skipped, not scored 0.
- **Store.** `EnrichmentRepo.LatestEmbeddings` — latest `embedding` payload per
  live (non-tombstoned) artifact, via a window function.
- **API.** `GET /search?q=...&k=...` (bearer-auth), returning ranked results with
  artifact title/url for display.

**Review fixes applied (Phase 3 Stage 1):**

- **Query text honors the privacy gate.** The searcher carries the same locality
  flag as the enrichers; a non-loopback embed endpoint makes `Semantic` refuse
  (`ErrRemoteDisallowed` → 503) so a search query never leaves the machine —
  closing the egress channel the enrichers already gate sensitive content off.
- **Non-finite / non-positive scores are skipped.** A pre-validation embedding row
  with a NaN component would corrupt the sort (strict-weak-ordering violation) and
  truncate the JSON response; such scores, and anti-correlated (cosine ≤ 0) hits,
  are dropped. `writeJSON` now encodes to a buffer first, so any marshal failure
  is a clean 500 rather than a half-written 200 — a fix for every endpoint.
- **Bounded memory.** `ForEachLatestEmbedding` streams candidates and the searcher
  keeps only a top-k min-heap (O(k)), instead of materializing every vector — no
  per-query memory cliff at scale.
- **Tier exposure is a documented decision.** Search is intentionally unfiltered
  by privacy tier (it serves the single owner behind the bearer token); a future
  lower-trust read path must add a tier ceiling in `ForEachLatestEmbedding`.

## Phase 3 — Stage 2: FTS5 keyword search

**Goal:** full-text keyword search over artifact title/body (design §8.8), the
keyword half of the eventual hybrid blend.

**Status: Complete.** Verified end to end (ingest → fetch → `GET /search?mode=keyword`
finds the artifact by its fetched body) plus unit coverage.

- **Build tag.** FTS5 needs `-tags sqlite_fts5` (mattn compiles the module only
  then). Defaulted in the Makefile (`GO_TAGS ?= sqlite_fts5`), set via `GOFLAGS`
  in CI, and added to `.golangci.yml` `build-tags`. All `go` commands now require
  it — the migration that creates the index fails without it.
- **Local index (migration 0004).** `artifacts_fts` FTS5 virtual table
  (`unicode61`, diacritic-folding). Per §6.0 it is a local index rebuilt from the
  synced `artifacts` table, excluded from the future CRR set. Standalone (not
  external-content) so it doesn't depend on rowid; backfills existing rows.
- **Trigger-free maintenance.** `ArtifactRepo.reindexFTS` rebuilds an artifact's
  FTS row from its current live state inside the same write transaction (called
  from `upsert` and from `Patch` when the body changes), so no DB triggers sit on
  the soon-to-be-CRR `artifacts` table (§6.0). Soft-deletes need no FTS write —
  the search join filters `deleted_at IS NULL`.
- **Search.** `ArtifactRepo.SearchKeyword` runs the FTS5 `MATCH` + `bm25` (negated
  so higher = better), joined to live artifacts. `search.Searcher.Keyword`
  reduces user input to quoted word terms (implicit AND) — defending against FTS5
  syntax errors/injection — and needs no embedder or locality check (the query
  never leaves SQLite).
- **API.** `GET /search?...&mode=semantic|keyword` (default semantic).
- **Fetch bug fixed in passing.** `htmlToMarkdown` left the `<head>` in the body,
  so the page title leaked into — and corrupted the tokens of — the body text. It
  now strips `<head>` after extracting the title.

## Phase 3 — Stage 3: Hybrid ranking

**Goal:** blend semantic and keyword results into one ranking (design §8.8), the
default search mode.

**Status: Complete.**

- **Reciprocal Rank Fusion.** `Searcher.Hybrid` pulls `fuseDepth` (50, capped at
  maxK) candidates from each ranker and fuses by RRF (`1/(rrfK+rank)`, rrfK=60).
  RRF is rank-based, so it combines the two incomparable score scales (cosine vs
  bm25) without normalization; ties break by artifact id.
- **Graceful degradation.** Keyword runs always (local, reliable); semantic is
  best-effort — if it is unavailable (non-local embed endpoint, or the model is
  down) Hybrid fuses keyword alone instead of failing.
- **API.** `mode` now defaults to `hybrid` (was semantic); `semantic` and
  `keyword` remain explicit options. Note the `score` scale differs by mode
  (cosine for semantic, bm25 for keyword, RRF sums for hybrid) — clients should
  rank by it, not threshold on an absolute value.
- **Review fix.** Hybrid degrades to keyword only when semantic is _legitimately_
  unavailable (non-local endpoint / model down); a cancelled or expired context
  propagates as an error rather than fabricating a degraded `200`.

## Phase 3 — Stage 4: RAG (question answering)

**Goal:** answer a question over the knowledge base, grounded on retrieved
artifacts, with mandatory citations back to sources (design §8.8).

**Status: Complete.** Verified end to end (ingest → `GET /ask` returns a
synthesized answer citing the ingested artifact).

- **`internal/rag`.** `Engine.Answer` retrieves the top-k via the hybrid
  `Retriever`, loads each artifact's body, builds a numbered-source prompt
  (rune-safe excerpts), and has the local LLM synthesize an answer instructed to
  cite every claim inline as `[n]`. Returns `{query, answer, sources[]}` where the
  source list position is the cited `[n]`. No relevant sources → a stock answer,
  no model call.
- **Privacy: on-device required.** Synthesis sends artifact bodies _and_ the
  question to the model — exactly what the gate keeps off a remote endpoint
  (§8.17). A non-local endpoint is refused (`ErrLocalRequired` → 503) rather than
  partially answered, since silently dropping sensitive sources would mislead.
- **API.** `GET /ask?q=...&k=...` (bearer-auth) returns the answer + sources; 400
  on empty/too-long query, 503 when unconfigured or non-local. Reuses the
  generation model (`KB_SUMMARIZE_MODEL`).

**Review fixes applied (Phase 3 Stage 4):**

- **Tombstone leak closed.** Retrieval now loads sources via `Artifacts.GetLive`
  (a `deleted_at IS NULL`-scoped accessor), so a delete racing ranking can't feed
  a soft-deleted artifact's body into the prompt — the highest-value leak surface.
- **Empty model answer is an error** (was a misleading `200` with sources and no
  text), matching `summarize`'s empty-output guard.
- **Prompt-injection hardening.** Source bodies are fenced and the model is told
  to treat them as data, not instructions (a mitigation, not a guarantee).
- **Citations are instructed, not verified** — documented honestly; validation is
  deferred to rerank.
- **Refactors.** Extracted the duplicated rune-safe truncation into
  `internal/textutil.TruncateBytes` (used by summarize/embed/rag); extracted the
  shared `?q`/`?k` parsing into `parseQueryAndK`; renamed `maxSearchQueryBytes` →
  `maxQueryBytes`; fixed the stale `Handler` auth doc.

This completes Phase 3 retrieval (semantic, keyword, hybrid, RAG).

## Phase 3 — Stage 5: Exactly-once enrichment writes

**Goal:** a crash can no longer duplicate a stage's enrichment rows (the
at-least-once limitation noted in Stage 2.x).

**Status: Complete.** The pipeline handler does its slow work (fetch/LLM) outside
any transaction, then commits the stage's effects (enrichment rows + artifact
patch) **and** the job's `MarkDone` in one short transaction. Atomicity makes it
exactly-once: a crash before commit re-runs the stage cleanly (nothing was
written); a commit lands the rows with the `done` status, and a `done` job is
never reclaimed, so a re-run cannot duplicate writes. The runner's own subsequent
`MarkDone` becomes a harmless no-op.

**Crash-window residual closed (review fix).** A crash between a job's atomic
commit and its post-completion finalize could leave an artifact in `enriching`
with all jobs terminal. `Pipeline.Reconcile` (run once at startup, after
`ReclaimStuck` so a re-pending job is excluded) finalizes any such artifact via
the same idempotent `finalizeArtifact` the on-complete hook uses. The strengthened
handler test now also asserts a true rollback: a failed enrichment write rolls
back the valid rows _and_ the `MarkDone` together (atomicity), not just that a
done job isn't reclaimed.

**Deferred (future phases):** "rerank" between retrieve
and synthesize (a cross-encoder/LLM rerank pass with citation validation —
currently the hybrid order is used directly); chunk-level embeddings (Decision 3
chunking) for finer RAG grounding; cr-sqlite peer sync (Phase 10).

## Phase 4 — Stage 1: Engagement + re-scoring (§9 living profile, part 1)

**Goal:** activate the relevance factors that were dormant because scoring ran
only once at first ingest (§8.9): the encounter factor (re-encounter) and the
engagement boost (`/engage`).

**Status: Complete.** Verified end to end (ingest → score; engage → re-score →
higher score).

- **Re-score = re-run the relevance stage standalone.** A fresh encounter
  (re-ingest) or an engagement enqueues a `relevance` job with no deps (the
  embedding already exists), which recomputes the score from the now-current
  signals (encounter count, `last_engaged_at`). The stage name is one const
  (`enrich.RelevanceStage`); the composition root supplies the enqueue closure via
  `api.RescoreFunc`, so the API and pipeline stay decoupled.
- **Engagement.** `POST /engage {artifact_id}` stamps `last_engaged_at`
  (`Artifacts.RecordEngagement`, live-scoped), re-scores in the same transaction,
  and publishes `engagement.recorded`; 404 for an unknown/tombstoned artifact.
- **Re-encounter.** Re-ingesting a known artifact now re-scores (the encounter
  factor changed) instead of doing nothing.

**Review fixes applied:**

- **Re-score is a distinct job type (`pipeline.RescoreJobType`), not a pipeline
  stage.** A re-score reuses the relevance handler but is excluded from
  enrichment-completion tracking: the finalize hook skips it and
  `ArtifactPipelineState` ignores it (`excludeType`). This fixes a real bug — a
  re-score job's completion was re-firing `finalize`, which re-published lifecycle
  events on every engagement and, after the original jobs were purged, could
  launder a `failed` artifact to `ready`.
- **Re-scores coalesce.** The enqueue (in the engagement/re-encounter
  transaction) skips when a non-terminal re-score for the artifact already exists
  (`Jobs.HasUnfinishedType`), so a scroll-happy UI or hot re-ingest can't pile up
  duplicate jobs.
- **`/engage` polish:** 413 (not 400) on an over-limit body; `artifact_id`
  validated for UTF-8/control chars.

**Known limitation — enrichment rows are unbounded.** Each re-score appends a new
`relevance` enrichment row (versioned), and unlike the jobs table enrichments have
no retention/purge. Coalescing bounds bursts, but sustained engagement still
accumulates rows. A retention policy (keep latest K per `(artifact, kind)`, or
prune old versions) is deferred.

## Phase 4 — Stage 2: Living profile (§9 part 2)

**Goal:** the relevance profile evolves from engagement, not just the static
`KB_INTERESTS` (§8.9) — engaging with topic X raises future X artifacts' scores.

**Status: Complete.**

- **Derived, not persisted.** The interest vectors a candidate is matched against
  are the static `KB_INTERESTS` vectors **plus the embeddings of the recently
  engaged artifacts** (`relevance.Profile.MatchWith`). No new table or migration —
  it derives from data already in the store (`last_engaged_at` + the embedding
  enrichment rows), so it survives restarts for free and stays consistent with the
  rest of the model.
- **`store.EnrichmentRepo.EngagedEmbeddings`** returns the latest embedding per
  live engaged artifact, most-recently-engaged first, capped (`maxEngagedInterests
= 32`) and excluding the artifact being scored (no self-match). `relsignals`
  parses them into vectors via the extended `SignalProvider`.
- **Discounted (`engagedInterestWeight = 0.85`)** so an explicitly configured
  interest stays authoritative, while a dimension-mismatched engaged vector
  contributes 0 (harmless). Combined with Stage 1's re-score triggers, engaging an
  artifact reshapes relevance for the whole corpus on the next score.

**Review:** clean (no P1/P2). The `EngagedEmbeddings` SQL, the bounded/stable
match math, the `Match`→`MatchWith(nil)` refactor, and the interface change were
all confirmed sound; the privacy path is unreachable (`relevance_score` is never
serialized). Realized: documented the tier-ceiling invariant on
`EngagedEmbeddings` (sharper than search's — the influence is baked into the
stored score). Noted below for the decay sweep.

## Phase 4 — Stage 3: Recency-decay sweep

**Goal:** old, untouched artifacts fade in ranking over time (§8.9) — the recency
factor is live but the score only recomputes on ingest/engage/re-encounter, so a
background pass is needed for time-based decay.

**Status: Complete.**

- **`internal/decay.Sweeper`** re-scores every live artifact in place. It applies
  the review's two guidances: the living-profile engaged set is loaded **once** per
  pass (not per artifact), and it **patches `relevance_score` only — no enrichment
  rows** — so running it on a schedule does not grow the enrichments table. A
  recomputed score within `scoreEpsilon` of the current one is skipped, so a sweep
  writes only the artifacts whose relevance actually moved. Context-cancellation
  aware; reuses the shared `relevance.AgeHours` (extracted from the enricher).
- **`cmd/kbd`** runs it on a `decayInterval` (6h) ticker, best-effort (logs and
  continues on error), stopping on shutdown.

This completes the §9 living-profile arc: relevance is now fully alive — profile
match (static + engaged), recency (with a decay sweep), engagement, encounter, and
source weight, recomputed on events and on a schedule.

**Review fixes applied (Stage 3):**

- **Right decay clock.** The recency factor now decays from `RecencyAnchor()` =
  `last_engaged_at` (or `first_seen_at` if never engaged), not `last_seen_at`. A
  re-sighting from a feed bumps `last_seen_at` and previously reset decay, so a
  stale-but-still-sighted, unengaged artifact never faded — defeating the feature.
  Recency now tracks user-staleness; re-sighting stays the encounter factor's job.
  Applied to BOTH the enricher and the sweep so they agree.
- **Skip-and-continue.** A per-artifact error (corrupt embedding, failed patch) is
  collected (`errors.Join`) and the sweep continues — one poison row can't wedge
  the whole corpus. Only ctx cancellation aborts. Dim-mismatched embeddings are
  skipped rather than scored to ~0.
- **Startup sweep.** Runs once at startup (then every 6h) to fix scores that went
  stale across downtime.

**Deferred:** enrichment-row retention; rerank + citation validation; chunk-level
embeddings; a partial `last_engaged_at` index if the engaged query gets hot;
cr-sqlite peer sync (Phase 10).

## Phase 5 — Source connectors (PULL)

The daemon could only receive **pushed** content (`POST /ingest`). Source
connectors let it **pull** on a schedule (§8.1). The `sources.Registry` (connector
name → `kb.SourceConnector`) was already the seam.

### Stage 1: Shared ingest path

**Status: Complete.** Extracted the write core out of the HTTP handler into
`internal/ingest`: an `Ingester.Ingest(Item)` that upserts (URL or content-hash
dedup), records the encounter, and atomically starts the pipeline (fresh insert)
or re-scores (re-encounter), publishing `artifact.ingested` post-commit. The
`Enqueuer`/`RescoreFunc` types and the `ValidText` guard moved here too, so push
and pull sources land **identically** (Principle 1) — the API handler now just
validates/canonicalizes and delegates. Behavior-preserving (all existing API
tests pass) plus direct tests of the seam (insert→enqueue, re-encounter→re-score,
content-hash dedup).

**Next (Stage 2):** the scheduler — a background loop that, per enabled PULL
source due for refresh, calls the registered connector's `Fetch(since cursor)`,
ingests each `RawItem` via the shared path, and persists the new cursor + health.
Plus one concrete connector (RSS/Atom) and a cursor column.

### Stage 2: Scheduler + RSS connector

**Status: Complete.**

- **Contract.** `SourceConnector` now takes the per-source `*Source` in `Fetch`/
  `HealthCheck` (one connector instance serves many sources — feed URL/auth/cursor
  come per-call), and `ID()` → `Name()`.
- **Fetch state (migration 0005).** `sources.cursor`, `last_fetched_at`, `health`
  columns; `SourceRepo.ListEnabledByKind` (the work list) and `UpdateFetchState`.
  `kb.Health` gained stable lowercase JSON tags.
- **`sources.Scheduler`.** `Run` ticks every `sourcePollInterval` (1m); `Tick`
  polls each enabled PULL source whose schedule TTL (`{"every_seconds":N}`,
  default 1h) has elapsed, calls the registered connector's `Fetch(src, cursor)`,
  ingests each item through the **shared `ingest.Ingester`** (so pull == push),
  and persists the new cursor + time + health. A per-source error is collected and
  the rest still run; an unknown connector is skipped, not fatal; a fetch error
  keeps the old cursor and records unhealthy.
- **`sources/rss`.** RSS 2.0 + Atom parser over an SSRF-guarded client
  (`ssrf.HTTPClient`). Returns items newer than the cursor (advancing it to the
  newest item's time); items carry the article link so the fetch stage retrieves
  full content; date-less items only on the first poll (so later polls don't
  re-encounter them).
- **`cmd/kbd`** registers the `rss` connector and runs the scheduler.

This closes the biggest functional gap: the daemon now fetches on its own.

**Review fixes applied:**

- **No same-second item loss (P1).** The cursor boundary is now inclusive
  (re-emit items AT the cursor second, deduped at ingest) instead of strictly
  after — a second item published in the same second as the last cursor is never
  dropped. Date-less items are always emitted (ingest dedup absorbs repeats) so a
  date-less feed doesn't go dark.
- **Encounter inflation fixed at the signal (P2).** The relevance encounter factor
  now counts **distinct sources** (`DistinctSourceCountForArtifact`), so re-polling
  one feed can't masquerade as "seen from N sources" — making the inclusive cursor
  and any re-emission inert for relevance by construction.
- **Content-bearing items aren't dropped (P2).** `toItem` falls back to a note when
  the URL is missing/uncanonicalizable; the connector resolves relative `<link>`s
  against the feed URL.
- **Hardening (P3):** reject non-http(s) feed URLs early; capture Atom
  `type="xhtml"` content via innerxml; bound the health `Detail` string.

### Stage 3: Source management API

**Status: Complete.** PULL sources could only be created programmatically, making
the feature unusable end to end. Added:

- **`POST /sources`** (bearer-auth): create a source `{kind, connector, config,
schedule?, default_privacy?}`. Validates the kind, that the connector is one the
  daemon registered (`api.ConnectorRegistry`, satisfied by `*sources.Registry` —
  an unusable source can't be created; the error lists available connectors), and
  that config/schedule are valid JSON. Returns the persisted source (201).
- **`GET /sources`**: list configured sources with `enabled`, `last_fetched_at`,
  and the `health` snapshot from the last poll — the operator can see what's
  running and whether it's healthy.

You can now add an RSS feed and watch it: `POST /sources {connector:"rss",
config:{url:...}}` → the scheduler polls it → `GET /sources` shows health.

**Review fixes applied:** config must be a JSON _object_ (not just valid JSON, so
a connector can't be handed a scalar config and become permanently unhealthy);
the source's declared kind must match its connector's kind (`Registry.Kind`),
rejecting an inert dead row; `Sources.Create` now writes back the schema defaults
(`enabled`, `source_weight`) so the handler returns the persisted source without a
re-read (removing the 500-after-success window); 413 on an over-limit body
(parity with ingest).

**Deferred:** more connectors (HN, GitHub, email…); source update/delete
endpoints; `storage_budget` enforcement; a `source_weight` yield feedback loop;
the failed-poll-delays-retry-by-a-full-TTL trade-off (a separate error-retry
floor).

---

# Phase 6 — Output / Render layer (§8.14)

The cyclic model's output half: a set of artifacts becomes a single assembled
Markdown document, then a `Renderer` turns that document into a target format.
Because bodies are already Markdown, the whole layer rides one contract —
`artifact(s) -> assembled Markdown -> format` — so a new format is a new Renderer
plus a registration. Every render is privacy-filtered and recorded.

## Stage 1: Render core (assembly + Markdown renderer + dispatch)

**Status: Complete.** The in-process foundation, no subprocess:

- **Privacy ordering** (`kb.PrivacyTier.Rank/AtMost`, `kb.MaxTier`): the
  sensitivity order the export filter and `privacy_max` are defined in. An
  unknown tier ranks above everything, so it is excluded by any ceiling rather
  than silently treated as public.
- **`render.Assemble`** (pure): artifacts → one Markdown document (H1 title, then
  per artifact an H2, the source URL as a CommonMark autolink, the body). Heading
  whitespace is collapsed so a multi-line title can't break the document.
- **`render.Markdown`** renderer (implements `kb.Renderer`): writes the assembled
  document to `<dir>/<name>.md`, owner-only (a render may hold restricted
  content), with a basename-traversal guard on the caller-supplied name.
- **`render.Registry`**: format → Renderer (mirrors the connector registry).
- **`render.Service`**: resolves artifact ids (dropping tombstoned ones, like
  retrieval), applies the privacy ceiling, dispatches to the renderer, and
  persists a `RenderRecord`. The render id names the output file, so file and
  provenance row share one identity.
- **`kb.RenderRecord` + `store.RenderRepo`** over the existing `renders` table;
  migration `0006` extends the `format` CHECK with `markdown` (the table rebuilt,
  it has no foreign keys in either direction). `artifact_ids` round-trips as a
  JSON array.

## Stage 2: pandoc + patat renderers

**Status: Complete.** The format renderers over the same assembled document:

- **`render.Pandoc`** (html/pdf/docx/epub): pipes assembled Markdown through the
  `pandoc` subprocess. One instance per format (the Renderer contract is
  single-format); `pandocFormats` maps each to its `-t` writer/extension/`-s`
  flag (pdf has no writer — the `.pdf` extension selects it). Security: content
  travels on **stdin**, never argv, so no body/title can inject a flag; a missing
  binary maps to `ErrRendererUnavailable` (→ 503, not 500); output is chmod'd
  owner-only after pandoc writes it. The `run` field is the exec seam — unit
  tests inject a fake; `TestPandoc_RealRoundTrip` exercises real pandoc when
  present (it is, in this env). `PandocAvailable()` lets the daemon warn at start.
- **`render.Patat`** (terminal slides): pure assembly (patat is an interactive
  presenter, not a converter — the artifact is the deck file). YAML metadata
  header (title + `wrap: true`), one slide per artifact separated by horizontal
  rules. A body's own thematic break (`---`) is neutralized to an em-dash rule so
  it can't fracture an artifact across slides; the title is YAML-quoted so a
  `:`/`"` can't break the header.

## Stage 3: Render API + daemon wiring

**Status: Complete.** The render layer is usable end to end:

- **`KB_RENDER_DIR`** config (default: a `renders/` dir beside the DB, inheriting
  the owner-only umask).
- **`POST /render`** (bearer-auth): `{format, artifact_ids, title?,
source_query?, privacy_ceiling?}` → runs the render synchronously and returns
  the persisted record (201) whose `output_path` names the file. Maps
  `ErrUnknownFormat`/`ErrInvalidCeiling` → 400, `ErrRendererUnavailable` → 503
  (pandoc not installed), other → 500 via `serverError` (generic client message,
  detail logged owner-only — closes the Stage 2 L2 stderr concern). Validates
  format, non-empty bounded `artifact_ids`, title/query lengths, and ceiling.
- **`GET /renders`**: render history (newest first).
- **Daemon wiring** (`cmd/kbd`): one Renderer per format (markdown + patat +
  pandoc html/pdf/docx/epub) into `cfg.RenderDir`; warns at startup if pandoc is
  absent.

Verified end to end against the real daemon binary: ingest a note → `POST
/render` produced a correct `.md`, an `.html` with pandoc-rendered `<strong>`, and
a 68 KiB `%PDF-1.7` via pandoc+TeX — all files `0600`, all listed by `GET
/renders`.

**Deferred:** async render-as-a-job (the `renders` table + job queue exist) for
slow conversions; serving render bytes over HTTP (`GET /renders/{id}/content`);
render inputs from a saved search/digest rather than explicit ids; audio
(radio_hn TTS+ffmpeg) renderer.

---

# Phase 7 — Surfacing engine (§8.11)

The output half of the cyclic loop (§5): select "what should I see now" from the
scored corpus, so it can be reviewed or rendered into a digest. Composes Phase 4
(relevance) with Phase 6 (render): §8.14 names digests a render input.

## Stage 1: Surface selector + endpoint

**Status: Complete.**

- **`store.ArtifactRepo.Surface(SurfaceQuery)`**: live, **scored** artifacts
  (`relevance_score IS NOT NULL`) ranked by relevance desc, then recency
  (first_seen_at desc), then id. Filters: status, a `Since` lower bound on
  first_seen_at (new arrivals), a `MinScore` floor, a privacy-tier whitelist, and
  a `Limit` (default cap 50).
- **`surface.Surfacer`**: wraps the query — always `status=ready` (only enriched,
  scored artifacts surface) and translates a privacy ceiling into the tier
  whitelist (empty/invalid ceiling = all tiers).
- **`GET /surface`** (`limit`, `since`, `min_score`, `privacy_ceiling`): ranked
  items (id, title, url, score, tier, first_seen_at). The ceiling defaults
  conservatively to `personal` (same fail-safe as the render export), so an
  omitted filter never surfaces sensitive/restricted content. Wired in the daemon.

## Stage 2: Digest rendering

**Status: Complete.** `POST /digest` `{format, title?, limit?, since?, min_score?,
privacy_ceiling?}` surfaces the top-ranked artifacts and renders them into one
document — the cyclic loop's delivery step, built on §8.14 ("digests are render
inputs"). The relevance rank order is preserved into the assembled document. The
same privacy ceiling gates both the surfacing and the render (defense in depth)
and defaults to `personal`. An empty surface → 422; render errors map via the
shared `writeRenderError` (also refactored out of `/render`). `source_query` is
recorded as `digest` so `GET /renders` distinguishes digest outputs.

## Stage 3: Spaced repetition (SM-2) + review queue

**Status: Complete.** The `review_schedule` table goes live:

- **`review.Schedule`** (pure SM-2): a recall grade 0..5 advances the schedule —
  ease adjusted per grade and floored at 1.3; a grade <3 lapses (reps/interval
  reset, ease still decays); 3+ gives 1 day, then 6, then `round(interval ×
ease)`. Grades are clamped.
- **`store.ReviewRepo`** (Upsert/Get/Due) over `review_schedule`; `Due(now,
limit)` returns schedules with `next_review_at <= now`, soonest first.
- **`review.Service`**: loads the prior schedule (default if new), applies SM-2,
  sets `next_review_at = now + interval`, persists; `Due` joins with the artifact
  for display, dropping tombstoned ones. Clock is injectable (tested).
- **`POST /review`** `{artifact_id, grade}` → updated schedule (404 for an
  unknown artifact, 400 for a grade outside 0..5). **`GET /review/due`** → the
  queue. Wired in the daemon.

**Deferred:** auto-seed a schedule on first engagement (engage → review-soon); a
daily-queue cap (5–10) and serendipity/trend signals (§8.11); surfacing
due-for-review items into the digest.

This completes Phase 7 (surfacing): surface selector, digest rendering, and
spaced repetition — the cyclic loop's delivery half over Phase 4 relevance and
Phase 6 render.

---

# Phase 8 — Knowledge graph (§8.7)

Entities and typed edges across artifacts, with N-hop traversal. The graph tables
(entities, artifact_entities, links, topics) exist from Phase 0; this phase fills
them.

## Stage 1: Graph store layer

**Status: Complete.** The pure persistence + traversal layer (no LLM):

- **`store.GraphRepo`**: `UpsertEntity` (dedup keystone — `ON CONFLICT(kind,name)`
  returns the canonical id, merges aliases without wiping); `LinkArtifactEntity`
  (idempotent artifact↔entity); `EntitiesForArtifact` / `ArtifactsForEntity`
  (live artifacts only); `AddLink` (directed edge, deduped on (src,dst,relation)
  via migration `0007`'s unique index + INSERT OR IGNORE); `NeighborIDs`
  (undirected adjacency over a frontier, live only); `ConnectedWithin(start,
hops)` (bounded BFS with a visited set, excludes the start).
- **`kb.Entity` / `kb.Link`** domain types.

## Stage 2: Entity-extraction enricher

**Status: Complete.** `enrich.Entities` is a pipeline stage (DependsOn fetch,
privacy-gated like summarize): one local-model call extracts
person/org/technology/concept/place entities, which are deduped into the graph
and linked to the artifact; artifacts sharing an entity gain a canonical
(direction-deduped) `shares_entity` edge — the graph builds itself during
enrichment. The model output is sanitized (blank/over-long names and
out-of-vocabulary kinds dropped, entity/alias counts capped). Graph writes are
idempotent, so the stage is safe to re-run; it records completion with an
`entities` enrichment row and writes the graph via the narrow `GraphWriter`
interface (satisfied by `*store.GraphRepo`). Wired into the daemon pipeline.

**Next:** Stage 3 — graph query API (`GET /graph/{id}?hops=N` connected subgraph;
backlinks).
