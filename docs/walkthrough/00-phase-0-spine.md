% KB Core — Phase 0: The Spine
% Walkthrough
%

# Phase 0: The Spine

**What:** the core daemon skeleton every later phase builds on — schema +
migrations, the storage layer, the durable job queue, the event bus, and an
authenticated HTTP API ending in a walking-skeleton `/ingest`.

**Why (design §12):** Phase 0 is ordered first by technical dependency — you
cannot index before you store, enrich before you have a queue, or surface before
you have an event bus. It validates that the seams fit before any vertical
feature is built on top.

## The shape

```
            cmd/kbd  (daemon: config -> open/migrate -> runner loop -> HTTP serve)
                |
   +------------+-------------------------------+
   |            |              |                |
 config       store          jobs             api
 (env)     (SQLite proj.   (queue +         (HTTP: /healthz,
            of the CRDT     DAG runner)      /events SSE, /ingest)
            knowledge base)      \             /
                                  \           /
                                   events.Bus (in-process fan-out)
```

## The packages

- **`internal/config`** — loads configuration from the environment
  (`KB_DB`, `KB_API_TOKEN`, ...), failing fast and naming every missing required
  variable. Local-first defaults (`127.0.0.1`).

- **`internal/store`** — the persistence layer (design §6): a SQLite database
  that is a materialized projection of the CRDT knowledge base (§6.0). Typed
  repositories over the synced tables (`Source`, `Artifact`, `Encounter`,
  `Enrichment`) plus the local-only `Job` queue. Keystones realized here:
  - **One artifact, many encounters** — `Artifact.UpsertByCanonical` returns the
    existing live artifact for a known canonical URL (atomic `INSERT … ON
CONFLICT … RETURNING`), so the same content seen from many sources is one
    artifact with many `encounter` rows (the dedup keystone, §8.2 / Principle 1).
  - **Transaction seam** — repos hold a `Querier` (satisfied by `*sql.DB` and
    `*sql.Tx`); `Store.Tx` runs several repos in one immediate transaction.
  - **cr-sqlite readiness** — client-minted ULIDs, tombstone soft-delete,
    repository-managed `updated_at` (no trigger), no logical-clock dependence on
    wall-clock timestamps (§6.0).

- **`internal/jobs`** — the durable enrichment runner (§5, Principle 4) over the
  local-only `jobs` table. `Claim` is a single race-free `UPDATE … WHERE id =
(SELECT … all deps 'done' …) RETURNING`; failures retry with exponential
  backoff then mark failed and **cascade-fail** dependents; crashes are recovered
  by a lease-aware `ReclaimStuck`. `Runner.Run` is the drain loop.

- **`internal/events`** — the in-process fan-out bus (§9, Principle 5). `Publish`
  never blocks: a slow subscriber drops events rather than stalling publishers.
  It is the system's nervous system.

- **`internal/api`** — the HTTP surface. Public `GET /healthz`; auth-wrapped
  (constant-time bearer token) `GET /events` (SSE; subscribes before flushing
  headers so a connected client misses nothing) and `POST /ingest`. Hardened with
  read/idle timeouts (no `WriteTimeout`, which would sever SSE) and an 8 MiB body
  limit.

- **`cmd/kbd`** — wires it all together and serves until SIGINT/SIGTERM, with a
  synchronous startup job-reclaim and graceful shutdown that joins the runner.

## The end-to-end flow: `POST /ingest`

This is the walking skeleton — the thinnest path that exercises every subsystem:

```
1. api: authenticate (bearer), cap + decode body, validate, look up source -> 400 if unknown
2. store.Tx (one atomic transaction):
     a. Artifacts.UpsertByCanonical  (insert or dedup to the existing artifact)
     b. Encounters.Record            (always: this sighting is a fact)
     c. Jobs.Enqueue                 (an 'ingest' enrichment job)
3. (after commit) events.Publish artifact.ingested  -> fans out to /events subscribers
4. jobs.Runner (background loop) claims the job and runs its handler
5. api: 200 { artifact_id }
```

The publish happens strictly **after** the transaction commits, so a rolled-back
ingest emits no phantom event (transactional-outbox in miniature).

## Decisions / trade-offs

- **CRDT peer sync (cr-sqlite primary)** drove the whole data model: synced
  knowledge vs local-only indexes/queue (§6.0), ULIDs, tombstones.
- **Local-only embeddings, structural RAG chunking, within-topic conflict
  scope** were chosen for privacy and simplicity (design §14).
- **Single-writer pool (`SetMaxOpenConns(1)`)** serializes writes and avoids
  `SQLITE_BUSY`; the read pool is deferred until read-heavy endpoints exist.

## What it is not yet

The `ingest` job handler is a placeholder; real fetching, canonicalization,
enrichment, search, and surfacing are later phases. Phase 1 makes ingestion
_correct_ — real URL canonicalization and content-hash dedup (§8.2).
