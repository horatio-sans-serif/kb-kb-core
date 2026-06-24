# Unified System Design Plan

Companion to `CONSOLIDATION_REPORT.md`. The report inventories 22+ projects, the
patterns worth keeping, and 14 gaps. This document turns that inventory into a
buildable architecture: a single data model, four plugin contracts, subsystem
specifications, and a dependency-ordered build sequence. Every gap in the report
maps to a subsystem here (see the traceability tables at the end).

The working name for the system is **KB** (the directory it lives in).

---

## Table of Contents

1. [Purpose and Scope](#1-purpose-and-scope)
2. [Vision and the Cyclic Model](#2-vision-and-the-cyclic-model)
3. [Design Principles](#3-design-principles)
4. [System Architecture](#4-system-architecture)
5. [The Cyclic Pipeline](#5-the-cyclic-pipeline)
6. [Core Data Model](#6-core-data-model)
7. [Plugin Contracts](#7-plugin-contracts)
8. [Subsystem Specifications](#8-subsystem-specifications)
9. [Event Bus and Triggers](#9-event-bus-and-triggers)
10. [Technology Stack](#10-technology-stack)
11. [Configuration Model](#11-configuration-model)
12. [Build Sequence (Dependency-Ordered)](#12-build-sequence-dependency-ordered)
13. [Migration Plan](#13-migration-plan)
14. [Design Decisions (Resolved)](#14-design-decisions-resolved)
15. [Traceability](#15-traceability)

---

## 1. Purpose and Scope

KB captures information from every channel a person uses, enriches it with AI
against a living model of that person's interests, stores it under one data
model, and makes it searchable, browsable, conversational, and self-surfacing.
It is local-first, single-user, and privacy-tiered.

This plan covers the full scope described in the consolidation report: all
ingress sources, the enrichment pipeline, storage and indexing, the knowledge
graph, retrieval (search + RAG), the surfacing/feedback loop, the output/render
layer, lifecycle management, multi-device access, and privacy. Nothing in the
report is deferred or descoped here; the build sequence in Section 12 orders the
work by technical dependency only (what must exist before what can function),
not by effort or perceived value.

---

## 2. Vision and the Cyclic Model

The report's central correction (Gap 14) is that the system is **a loop, not a
funnel**. A funnel ends at storage; a second brain closes the loop by surfacing
what it stored, observing what the user does with it, and feeding that back into
how it fetches, enriches, and surfaces.

```
            +-------------------------------------------------+
            |                                                 |
            v                                                 |
  SOURCES -> FETCH -> ENRICH -> STORE -> SURFACE -> ACT -> REFLECT
   ^  ^                  ^         ^         ^                 |
   |  |                  |         |         |                 |
   |  +-- fetch priority |         |         +-- engagement    |
   |      reshaped       |         |             signals       |
   |                     |         |                           |
   |                     |         +-- re-score archive <------+
   |                     |                                     |
   |                     +-- enrichment models refined <-------+
   |                                                           |
   +-- source weight adjusted by yield <----------------------+
                                                               |
                                          UPDATE LIVING PROFILE +
```

The profile is the hub of the loop. It is not a static text document declaring
interests; it is a versioned, weighted model that engagement mutates over time.
Every other subsystem reads it (to score relevance, to rank surfacing) and the
feedback subsystem writes it (from engagement signals).

---

## 3. Design Principles

1. **One canonical artifact, many encounters.** The same content seen from N
   sources is one `artifact` with N `encounter` rows. This is the keystone:
   deduplication, the multi-source relevance signal, and per-source health
   metrics all derive from it. (Report Gap 2.)

2. **Markdown at rest.** Every artifact's body is normalized to Markdown. This
   makes content human-editable (the `gg`/`notes` lineage), git-friendly, and
   directly consumable by the pandoc render layer with no extra conversion.

3. **Local-first, peer-replicated, single binary + sidecars.** A Go daemon owns
   the local database, the job queue, the event bus, the scheduler, and the HTTP
   API. The knowledge base is a CRDT replicated to every device (Decision 1):
   each device holds a full replica and edits merge automatically, offline.
   Heavy enrichment runs only on the **compute peer** (the Mac), which hosts the
   sidecars (headless browser, transcription, LLM); other devices capture and
   review, and the compute peer's enrichment results replicate back. No cloud
   dependency is required for core function.

4. **Durable, versioned, resumable enrichment.** Enrichment is a DAG of stages
   driven by a persistent job queue. A crash resumes mid-pipeline. Enrichment
   outputs are versioned so the archive can be re-enriched when models or the
   profile change.

5. **The event bus is the nervous system.** Lifecycle events (artifact ingested,
   enriched, engaged-with; source failed; conflict detected; review due) publish
   to an internal bus. Triggers, surfacing, context-awareness, and multi-device
   push are all subscribers. (Inherited from `haus` SSE + `shelf` triggers.)

6. **The profile is a living model.** Interests carry weights. Engagement updates
   weights. Profile changes trigger re-scoring jobs across the archive. (Gap 14.)

7. **Privacy is a property of every artifact and every code path.** A privacy
   tier on each artifact governs which LLM may process it (sensitive content
   never leaves the machine), what any export or API response may include, and
   what the audit log records. (Gap 12.)

8. **Extensibility through four contracts.** New sources, new content types, new
   enrichment steps, and new output formats are added by implementing one of
   four interfaces (Section 7). The core never changes to add a source.

---

## 4. System Architecture

### 4.1 Topology

```
                          +----------------------------+
                          |        KB CORE (Go)        |
  PUSH capture            |                            |
  (extensions, share      |  HTTP API + SSE event bus  |
   sheet, CLI, hooks) --->|  Scheduler (launchd-backed)|
                          |  Job queue (DAG runner)    |
  PULL connectors ------->|  Storage: SQLite + sqlite- |        +-------------+
  (scheduled fetch)       |          vec               |<------>| SQLite file |
                          |  Relevance + profile engine|        +-------------+
  AMBIENT capture  ------>|  Surfacing engine          |
  (tldw frames,           |  Render dispatcher         |
   photo/OCR)             |                            |
                          +----+-----+------+-----+----+
                               |     |      |     |
              local HTTP/exec  |     |      |     |
                               v     v      v     v
                       +-------+ +---+--+ +-+---+ +-----------+
                       |Ollama | |Play- | |DICT-| | pandoc /  |
                       |(LLM + | |wright| |ATOR | | patat /   |
                       |embed +| |fetch | |(STT)| | ffmpeg    |
                       |vision)| |side- | |     | | (render)  |
                       +-------+ |car   | +-----+ +-----------+
                                 +------+
```

### 4.2 The hub (KB Core, Go)

A single Go daemon. Go is chosen because `rememberer` already proves the
Go + SQLite + sqlite-vec + Ollama stack end-to-end (report Recommendation 1),
and a single static binary suits a local-first daemon. The core provides:

- **HTTP API** for all reads/writes from any device and any local tool.
- **SSE event bus** for push (digests, suggestions, conflict alerts, real-time UI).
- **Scheduler** that registers launchd jobs (the `arc-tabs-logger` pattern) and
  evaluates per-source refresh policies (the `unfurl` pattern).
- **Job queue + DAG runner** that drives the enrichment pipeline with retries
  and resume.
- **Storage layer** over a single SQLite database with `sqlite-vec` for vectors.
- **Relevance + profile engine**, **surfacing engine**, and **render dispatcher**.

### 4.3 Sidecars

| Sidecar                 | Runtime      | Responsibility                                                                               | Lineage                          |
| ----------------------- | ------------ | -------------------------------------------------------------------------------------------- | -------------------------------- |
| Ollama                  | local server | embeddings, summarization, relevance, vision (OCR / frame analysis), entity/claim extraction | rememberer, tldr-web, local-tldr |
| Playwright fetcher      | Node service | render JS-heavy pages to clean Markdown + screenshot                                         | tldr-web                         |
| DICTATOR                | external     | audio/video -> transcript + diarization + timestamps                                         | report Gap 13                    |
| pandoc / patat / ffmpeg | subprocess   | render artifacts to PDF/HTML/slides/audio                                                    | Output layer                     |

Sidecars are stateless relative to KB; the core owns all state. A sidecar being
down degrades one capability (e.g. transcription) without stopping the system;
affected jobs stay queued and retry.

### 4.4 Replication topology (CRDT peer sync, Decision 1)

```
   [iPhone replica] <-> [iPad replica]
          ^                   ^
          v                   v
   [Mac replica + sidecars] <-> [laptop replica]
     ^ compute peer: Ollama, Playwright, DICTATOR, pandoc
```

Every device runs the KB core over its own SQLite projection of the shared CRDT
knowledge base. Replicas gossip and merge over Tailscale when reachable and work
fully offline otherwise. Only the Mac runs the enrichment sidecars, so it is the
**compute peer**: it claims un-enriched artifacts created on any device, runs the
pipeline, and replicates enriched results back as ordinary synced data. A device
with no connectivity still captures, reads, and searches its local replica.

---

## 5. The Cyclic Pipeline

Each stage is one or more job types in the queue. Edges are dependencies enforced
by the DAG runner. Stages publish events on completion.

```
INGEST            create/merge artifact + encounter
  |
CANONICALIZE      URL normalization, redirect resolution
  |
DEDUP             content fingerprint -> merge or branch
  |
EXTRACT           content-type router -> Fetcher (web/pdf/image/media/...)
  |
TRANSCRIBE        (audio/video only) DICTATOR -> transcript artifact
  |
SUMMARIZE         summary + tags + critical analysis (structured LLM output)
  |
EXTRACT-ENTITIES  people/orgs/tech/concepts -> entity registry + links
  |
ASSESS-RELEVANCE  score against living profile
  |
EMBED             vectors -> sqlite-vec (artifact + RAG chunks)
  |
LINK              graph edges: shared URL/entity/topic, backlinks
  |
EXTRACT-CLAIMS    factual claims -> conflict detection
  |
CLASSIFY-LIFECYCLE assign active/archive tier, privacy tier default
  |
READY             event: artifact.ready  (now searchable + surfaceable)

                  ... later, asynchronously ...

SURFACE           digest / suggestion / spaced-repetition / serendipity
  |
ACT               user views / shares / annotates / acts / dismisses
  |
REFLECT           engagement recorded -> profile learning -> re-score jobs
```

Re-enrichment (when the profile shifts or models change) re-runs from
`ASSESS-RELEVANCE` onward without re-fetching, because the extracted Markdown
body is already stored.

---

## 6. Core Data Model

Each device holds a SQLite database. DDL below is the authoritative schema
sketch; types and constraints are real, indexes are indicative.

### 6.0 Sync classification (CRDT peer sync — Decision 1)

The data model splits into three classes by how it replicates:

**Synced (CRDT source of truth).** The knowledge itself, replicated to every
device and merged automatically: `sources`, `artifacts`, `encounters`,
`enrichments` (including embedding vectors as data), `entities`,
`artifact_entities`, `links`, `topics`, `artifact_topics`, `profile_interests`,
`profile_history`, `engagements`, `review_schedule`, `claims`, `conflicts`,
`renders`. SQLite is a local materialized projection of these CRDT documents:
all writes go through the CRDT layer, which then projects into SQLite for query.

**Local-only, rebuilt from synced data.** Derived indexes that are cheap to
regenerate and need not replicate: the `sqlite-vec` ANN index and the FTS5
index. Embedding _vectors_ sync (as `enrichments` data, so every replica can do
offline semantic search); the ANN index over them is rebuilt locally.

**Local-only, device-specific.** Runtime state that is meaningless to replicate:
`jobs` (the enrichment queue), scheduler state, sidecar state, and `audit_log`
(records access on _this_ device).

CRDT field-type mapping: scalar fields a single writer owns are last-writer-wins
registers (`title`, `status`, `privacy_tier`); accumulating collections are
grow-only / OR-sets (encounters, tags, links, engagements); soft-delete uses
tombstones (the braindrain pattern aligns with CRDT delete). IDs are
client-generated ULIDs so any device can mint records offline without
coordination.

**cr-sqlite readiness (before `crsql_as_crr` at Phase 10).** Choices that are
fine in single-node SQLite but must be revisited before promoting tables to
CRRs:

- **No app-level triggers that mutate CRR columns.** A merge writes rows
  directly; a local `AFTER UPDATE` trigger would overwrite merged values with
  local wall-clock and produce values that are neither replica's. `updated_at`
  is therefore set by the repository layer, not a trigger.
- **`datetime('now')` defaults are local wall-clock**, acceptable for audit
  timestamps (`created_at`) but never a logical merge clock — cr-sqlite supplies
  the causal metadata; do not treat these columns as ordering authority across
  replicas.
- **Hot-update columns** (`relevance_score`, `lifecycle_tier`) rewrite per-column
  CRDT metadata on every change; keep them out of unnecessary indexes (only the
  `lifecycle_tier, relevance_score` composite is kept).
- **The partial-unique index on `canonical_url`** dedups within one replica;
  cross-replica dedup is reconciled by the `same_as` merge at sync time (§8.2),
  not by the unique index.

### 6.1 Sources

```sql
CREATE TABLE sources (
  id              TEXT PRIMARY KEY,           -- e.g. "hn", "twitter-bookmarks"
  kind            TEXT NOT NULL,              -- PULL | PUSH | AMBIENT
  connector       TEXT NOT NULL,              -- registered Source implementation
  config          JSON NOT NULL,              -- connector-specific
  schedule        JSON,                       -- refresh policy (cron|ttl|rrule)
  auth_state      JSON,                       -- token/oauth state + expiry
  default_privacy TEXT NOT NULL DEFAULT 'personal',
  storage_budget  INTEGER,                    -- max artifacts retained (NULL = unbounded)
  source_weight   REAL NOT NULL DEFAULT 1.0,  -- adjusted by yield (feedback loop)
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
```

### 6.2 Artifacts (canonical knowledge items)

```sql
CREATE TABLE artifacts (
  id              TEXT PRIMARY KEY,
  type            TEXT NOT NULL,              -- webpage|pdf|video|audio|image|note|
                                              -- transcript|message|social_post|email|code|render
  canonical_url   TEXT,                       -- normalized; NULL for non-URL artifacts
  content_hash    TEXT,                       -- fingerprint for cross-URL dedup
  title           TEXT,
  body_markdown   TEXT,                       -- normalized content at rest
  privacy_tier    TEXT NOT NULL DEFAULT 'personal', -- public|personal|sensitive|restricted
  lifecycle_tier  TEXT NOT NULL DEFAULT 'active',    -- active|archive|purged
  status          TEXT NOT NULL DEFAULT 'ingested',  -- ingested|enriching|ready|failed
  relevance_score REAL,                       -- current, decays over time
  first_seen_at   TEXT NOT NULL,
  last_seen_at    TEXT NOT NULL,
  last_engaged_at TEXT,
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
-- Dedup indexes are partial-unique, scoped to LIVE rows, so a tombstone frees
-- the slot and the two upsert paths (canonical URL, content hash) never collide
-- (a URL artifact has a NULL content_hash until fetched; a body artifact has a
-- NULL canonical_url). relevance_score is a hot-decay column kept out of indexes;
-- it is ranked within a lifecycle tier via the composite below.
CREATE UNIQUE INDEX idx_artifacts_canonical ON artifacts(canonical_url)
  WHERE canonical_url IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX idx_artifacts_content_hash ON artifacts(content_hash)
  WHERE content_hash IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX idx_artifacts_lifecycle ON artifacts(lifecycle_tier, relevance_score DESC);
```

### 6.3 Encounters (the dedup keystone)

Every time an artifact is seen, from any source, one row is written here. The
artifact is created once; encounters accumulate. `COUNT(encounters)` is itself a
relevance signal ("seen from 4 sources").

```sql
CREATE TABLE encounters (
  id            TEXT PRIMARY KEY,
  artifact_id   TEXT NOT NULL REFERENCES artifacts(id),
  source_id     TEXT NOT NULL REFERENCES sources(id),
  raw_url       TEXT,                         -- the URL as seen (pre-canonicalization)
  context       JSON,                         -- source-specific: highlighted text, channel,
                                              -- sender, surrounding message, tweet id, etc.
  seen_at       TEXT NOT NULL,
  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_encounters_artifact ON encounters(artifact_id);
CREATE INDEX idx_encounters_source ON encounters(source_id);
```

### 6.4 Enrichments (versioned)

```sql
CREATE TABLE enrichments (
  id            TEXT PRIMARY KEY,
  artifact_id   TEXT NOT NULL REFERENCES artifacts(id),
  kind          TEXT NOT NULL,                -- summary|tags|critical_analysis|relevance|...
  payload       JSON NOT NULL,                -- structured output (Zod/JSON-schema validated)
  model         TEXT NOT NULL,                -- e.g. "llama3.1:8b"
  prompt_version TEXT NOT NULL,
  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_enrichments_artifact_kind ON enrichments(artifact_id, kind);
```

The "critical analysis" enrichment (from `summarize-url.sh`) is mandatory on
every text artifact: the LLM must identify weaknesses, assumptions, and missing
context. This prevents uncritical hoarding.

### 6.5 Embeddings (sqlite-vec)

```sql
-- artifact-level and chunk-level vectors for RAG
CREATE VIRTUAL TABLE embeddings USING vec0(
  artifact_id TEXT,
  chunk_index INTEGER,
  embedding   FLOAT[768]                      -- dim per chosen embed model
);
```

Embedding model is local-only Ollama (Decision 2): `nomic-embed-text` at 768-dim,
one vector space across all privacy tiers. Chunks follow Markdown structure with
a fixed-size fallback for oversized blocks (Decision 3). Vectors sync as
`enrichments` data; this `vec0` index is local and rebuilt per device (6.0).

### 6.6 Knowledge graph

```sql
CREATE TABLE entities (
  id      TEXT PRIMARY KEY,
  kind    TEXT NOT NULL,                       -- person|org|technology|concept|place
  name    TEXT NOT NULL,
  aliases JSON,
  UNIQUE(kind, name)
);

CREATE TABLE artifact_entities (
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  entity_id   TEXT NOT NULL REFERENCES entities(id),
  PRIMARY KEY (artifact_id, entity_id)
);

CREATE TABLE links (
  id          TEXT PRIMARY KEY,
  src_id      TEXT NOT NULL REFERENCES artifacts(id),
  dst_id      TEXT NOT NULL REFERENCES artifacts(id),
  relation    TEXT NOT NULL,                   -- references|shares_entity|shares_topic|same_as|derived_from
  weight      REAL NOT NULL DEFAULT 1.0,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_links_src ON links(src_id);
CREATE INDEX idx_links_dst ON links(dst_id);   -- backlinks

CREATE TABLE topics (
  id    TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  centroid JSON                                 -- topic vector for clustering
);
CREATE TABLE artifact_topics (
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  topic_id    TEXT NOT NULL REFERENCES topics(id),
  score       REAL NOT NULL,
  PRIMARY KEY (artifact_id, topic_id)
);
```

### 6.7 Living profile (versioned)

```sql
CREATE TABLE profile_interests (
  id          TEXT PRIMARY KEY,
  label       TEXT NOT NULL,                    -- "Rust async", "distributed systems"
  kind        TEXT NOT NULL,                    -- interest|project|employment|hobby
  weight      REAL NOT NULL DEFAULT 1.0,        -- mutated by engagement
  embedding   JSON,                             -- for semantic relevance scoring
  active      INTEGER NOT NULL DEFAULT 1,
  updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE profile_history (                  -- temporal: tracks interest drift
  id          TEXT PRIMARY KEY,
  interest_id TEXT NOT NULL REFERENCES profile_interests(id),
  weight      REAL NOT NULL,
  recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

### 6.8 Engagement (feedback signals)

```sql
CREATE TABLE engagements (
  id          TEXT PRIMARY KEY,
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  kind        TEXT NOT NULL,                    -- view|dwell|share|annotate|act|thumbs_up|
                                                -- thumbs_down|dismiss|more_like|less_like
  value       REAL,                             -- e.g. dwell seconds
  note        TEXT,                             -- correction explanation
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_engagements_artifact ON engagements(artifact_id);
```

### 6.9 Surfacing / spaced repetition

```sql
CREATE TABLE review_schedule (
  artifact_id   TEXT PRIMARY KEY REFERENCES artifacts(id),
  next_review_at TEXT NOT NULL,
  interval_days  REAL NOT NULL DEFAULT 1,        -- SM-2-style
  ease           REAL NOT NULL DEFAULT 2.5,
  reviews        INTEGER NOT NULL DEFAULT 0
);
```

### 6.10 Conflict detection

```sql
CREATE TABLE claims (
  id          TEXT PRIMARY KEY,
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  statement   TEXT NOT NULL,
  embedding   JSON,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE conflicts (
  id          TEXT PRIMARY KEY,
  claim_a     TEXT NOT NULL REFERENCES claims(id),
  claim_b     TEXT NOT NULL REFERENCES claims(id),
  status      TEXT NOT NULL DEFAULT 'open',      -- open|resolved|dismissed
  rationale   TEXT,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
```

### 6.11 Jobs, renders, audit

```sql
CREATE TABLE jobs (
  id          TEXT PRIMARY KEY,
  type        TEXT NOT NULL,                     -- pipeline stage name
  artifact_id TEXT REFERENCES artifacts(id),
  depends_on  JSON,                              -- job ids that must complete first
  payload     JSON,
  status      TEXT NOT NULL DEFAULT 'pending',   -- pending|running|done|failed
  attempts    INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT,
  run_after   TEXT,                              -- backoff scheduling
  claimed_at  TEXT,                              -- set on claim; lease for stuck-job reclaim
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_jobs_status ON jobs(status, run_after);

CREATE TABLE renders (
  id          TEXT PRIMARY KEY,
  format      TEXT NOT NULL,                     -- patat|pdf|html|docx|epub|audio
  source_query TEXT,                             -- the query/artifact set rendered
  artifact_ids JSON,
  output_path TEXT,
  privacy_max TEXT NOT NULL,                     -- highest tier included (export filter)
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE audit_log (
  id          TEXT PRIMARY KEY,
  actor       TEXT NOT NULL,                     -- device/api-key/local
  action      TEXT NOT NULL,
  artifact_id TEXT,
  privacy_tier TEXT,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
```

`★ Insight ─────────────────────────────────────`
This schema deliberately keeps three things separate that lesser PKM tools
conflate: the **artifact** (what the thing is), the **encounter** (when/where you
met it), and the **engagement** (what you did about it). Conflating artifact and
encounter breaks dedup; conflating encounter and engagement breaks the relevance
loop. Keeping them as three tables is what lets the feedback cycle in Section 5
be a few SQL queries instead of a rewrite.
`─────────────────────────────────────────────────`

---

## 7. Plugin Contracts

Four interfaces are the entire extensibility surface. Adding a source, a content
type, an enrichment step, or an output format means implementing one of these
and registering it; the core does not change.

```go
// SOURCE: a registered origin of artifacts. (shelf "one module per source")
type Source interface {
    ID() string
    Kind() SourceKind                                  // PULL | PUSH | AMBIENT
    Fetch(ctx context.Context, cur Cursor) (Batch, Cursor, error) // PULL: incremental
    HealthCheck(ctx context.Context) (Health, error)   // for source monitoring
    AuthState() AuthState                               // expiry detection
}

// FETCHER: turns a reference into extracted Markdown by content type.
// (bookmarks-manager content-type dispatcher; tldr-web Playwright)
type Fetcher interface {
    CanHandle(ref ResourceRef) bool                     // by MIME / URL shape
    Extract(ctx context.Context, ref ResourceRef) (Extracted, error)
}
// Extracted: { Title, BodyMarkdown, Media[], Metadata, Screenshot? }
// Graceful degradation: partial Extracted beats an error.

// ENRICHER: one stage in the pipeline DAG. (digidex/summarize-url.sh model)
type Enricher interface {
    Name() string
    DependsOn() []string                                // stage names
    PrivacyOK(tier PrivacyTier) bool                    // e.g. cloud LLM rejects 'sensitive'
    Enrich(ctx context.Context, a *Artifact) (Enrichment, error)
}

// RENDERER: artifact(s) -> output format. (pandoc/patat/radio_hn)
type Renderer interface {
    Format() string                                     // patat|pdf|html|docx|epub|audio
    Render(ctx context.Context, arts []Artifact, opts RenderOpts) (Rendered, error)
}
```

`SourceKind`:

- **PULL** — KB fetches on a schedule (RSS, Twitter bookmarks, YouTube history, shelf importers, arc-tabs-logger).
- **PUSH** — the user sends content in (browser extension, share sheet, CLI, chat hooks).
- **AMBIENT** — passive background capture (tldw frames, camera-roll watch).

---

## 8. Subsystem Specifications

Each subsystem lists the report gap(s) it closes and the existing project(s) it
draws from.

### 8.1 Source Connectors and Scheduling

_Gaps 1, 9. From: arc-tabs-logger, shelf, unfurl._

- Connectors implement `Source`. PULL connectors are driven by the scheduler;
  PUSH connectors hit the ingest API; AMBIENT connectors run as their own daemons
  feeding the ingest API.
- The scheduler evaluates per-source policies (cron, TTL, iCal RRULE) with
  exponential-backoff retry — the `unfurl` policy model, executed via launchd
  jobs registered by the core (the `arc-tabs-logger` pattern).
- Connector catalog (full scope, from report Gap 1 + shelf's importer list):
  browser tabs/bookmarks, Chrome/Firefox extension highlights, RSS/Atom,
  Twitter/X, Reddit, Mastodon, Bluesky, YouTube (history/likes/transcripts),
  podcasts, Slack, Discord, iMessage, Signal, Telegram, WhatsApp, email +
  attachments, Downloads-folder watch, Google Docs/Notion/Confluence, meeting
  recordings (Zoom/Meet), camera roll / photos, location context.

### 8.2 Ingestion, Canonicalization, Deduplication

_Gap 2. New._

- Ingest creates an `artifact` (or finds an existing one) and always writes an
  `encounter`.
- **Canonical URL resolver**: strip tracking params (`utm_*`, `fbclid`, ...),
  follow redirects, normalize host/protocol/trailing-slash, de-AMP.
- **Content fingerprint**: SimHash/MinHash over normalized body for same-content-
  different-URL cases (syndication, mirrors). Match -> merge into existing
  artifact, add encounter; otherwise create new.
- **Merge strategy**: union tags, annotations, and context across encounters;
  never discard the duplicate — record it as another encounter.

### 8.3 Content Extraction (Fetchers)

_Gaps 1. From: tldr-web, bookmarks-manager._

- A content-type router selects a `Fetcher`. Routes: web page (Playwright sidecar
  - Mozilla Readability), PDF (pdf extraction + OCR fallback), image (vision LLM
    OCR), audio/video (-> transcription, 8.4), GitHub repo, plain text/Markdown.
- Title priority: Open Graph > page title > URL > filename. Screenshot fallback
  when metadata is sparse. Partial extraction beats failure (graceful
  degradation).

### 8.4 Transcription (DICTATOR)

_Gap 13. External dependency._

- All audio/video routes through DICTATOR early, before any text enrichment.
- The transcript is a **first-class artifact** of type `transcript`, linked to
  its source via a `derived_from` edge.
- Speaker diarization and timestamp alignment are preserved in the transcript's
  structure (segments with speaker + start/end), enabling "jump to this moment."

### 8.5 Enrichment Pipeline

_Gaps 3, 6 (prep), 7 (prep). From: summarize-url.sh, digidex, tldr-web, rememberer._

- DAG of `Enricher` stages (Section 5). Structured, schema-validated LLM output
  for summary, tags, and the mandatory critical analysis.
- Entity extraction populates the entity registry and seeds graph links.
- Embedding produces artifact-level and chunk-level vectors for search and RAG.
- Privacy-aware routing: `PrivacyOK` lets a cloud enricher refuse `sensitive`/
  `restricted` artifacts, forcing local Ollama for those.

### 8.6 Storage and Indexing

_Store/Index. From: rememberer, braindrain, shelf, haus._

- Per-device SQLite (Section 6) is a materialized projection of the CRDT
  knowledge base. `sqlite-vec` (vectors) and FTS5 (fuzzy/title search,
  braindrain's dual-search) are local-only indexes rebuilt from synced data
  (6.0). Soft-delete uses CRDT tombstones (braindrain's TTL pattern). WAL mode
  for concurrent read while the daemon writes.

### 8.7 Knowledge Graph and Entities

_Gap 3. From: mindmapper (view)._

- Edges built during enrichment: shared canonical URL, shared entity, shared
  topic, explicit references, `same_as` (dedup), `derived_from` (transcripts,
  renders). Backlinks are just reverse-direction `links` queries.
- Topic clustering over embeddings assigns `artifact_topics`.
- Graph query API: "everything connected to X within N hops." Visual graph view
  reuses the `mindmapper` spatial UI.

### 8.8 Retrieval: Search and RAG

_Gap 6. From: braindrain, rememberer._

- **Search**: hybrid of FTS5 fuzzy/title + sqlite-vec semantic, with a ranking
  blend.
- **Chunking**: structural by Markdown block with a fixed-size fallback for
  oversized blocks (Decision 3). Chunk vectors live in `embeddings` (6.5).
- **RAG**: retrieve -> rerank -> synthesize with an LLM -> answer with mandatory
  citations linking back to source artifacts. Query types: temporal ("last month
  about Rust async"), comparative ("approaches to X I've seen"), synthesis ("key
  takeaways on Y"). RAG output can be handed to the render layer (8.14).

### 8.9 Relevance Model and Living Profile

_Gaps 5, 10, 14. From: digidex, summarize-url.sh, tldr-web2._

Relevance combines profile match, recency, engagement, multi-source signal, and
source weight:

```
relevance(a) = profile_match(a, profile)        // cosine vs weighted interest vectors
             * recency_factor(a.last_seen_at)
             * engagement_factor(a)              // boosts engaged items
             * encounter_factor(count(a))        // "seen from N sources"
             * source_weight(a)                  // sources that yield engagement
```

The profile is `profile_interests` with weights; scoring uses interest
embeddings. The profile is versioned (`profile_history`) so drift is observable.

### 8.10 Feedback Loop and Engagement

_Gaps 10, 14. New._

- Every interaction writes an `engagement` row (explicit thumbs/more-like/less-
  like/dismiss/correction; implicit dwell/share/annotate/act).
- A **profile-learning job** periodically reweights `profile_interests` from
  engagement, records `profile_history`, then enqueues **re-score jobs** across
  the archive (re-running `ASSESS-RELEVANCE`).
- Source weights are adjusted by yield: sources whose items get engaged with gain
  weight (and fetch priority); noisy sources lose it.
- Prediction-accuracy tracking: did the user engage with high-scored items and
  ignore low-scored ones? Used to tune the relevance blend.

### 8.11 Surfacing Engine

_Gap 5. New. Uses: tldw (context), event bus._

- **Morning digest**: scheduled job ranks overnight arrivals by relevance to
  active projects, renders via pandoc, delivers (web/push/email).
- **Spaced repetition**: SM-2 variant over `review_schedule`; engaged items
  lengthen intervals, dismissed items decay. Daily queue capped (5-10 items).
- **Context-aware suggestions**: subscribe to tldw activity events + active git
  repo + open files; push related artifacts over SSE.
- **Trend detection**: emerging-topic detection over recent encounters/
  engagements; offer to promote a topic to a tracked interest.
- **Serendipity**: occasional high-quality artifact from outside current focus.
- **Weekly review**: ingested vs engaged vs ignored, and how interests shifted.

### 8.12 Lifecycle and Garbage Collection

_Gap 8. New._

- **Relevance decay**: scores decay (exponential by time-since-engagement) unless
  refreshed by engagement or re-encounter.
- **Tiers**: active -> archive -> purge, by relevance + age.
- **Storage budgets per source** prevent one noisy feed from dominating.
- **Review prompts** for archive-tier items; **purge policy** for permanent
  deletion below a relevance threshold after N months with no engagement.

### 8.13 Conflict Detection

_Gap 7. New._

- Claim extraction during enrichment populates `claims`. New claims are compared
  (semantic + LLM judgment) against existing claims within the same topic cluster
  or N graph hops (Decision 5), reusing the search vector pre-filter to keep the
  comparison set bounded.
- Contradictions create `conflicts` rows and publish `conflict.detected`,
  surfacing for human resolution rather than silently storing both.
- Source credibility signals accrue from how often a source's claims survive
  conflict resolution.

### 8.14 Output / Render Layer

_Report "Output / Render Layer". From: radio_hn, pandoc/patat/Eisvogel._

- `Renderer` implementations: patat (terminal slides), Eisvogel (PDF), Bootstrap
  template (HTML), DOCX, EPUB, and audio (radio_hn's TTS + ffmpeg path).
- Because bodies are Markdown, `artifact(s) -> assembled Markdown -> pandoc ->
format` is the single contract. Digests (8.11) and RAG answers (8.8) are render
  inputs. Every render is privacy-filtered (records `privacy_max`).

### 8.15 Source Health Monitoring

_Gap 9. New. Uses: Source.HealthCheck / AuthState._

- Per-source fetch success/failure metrics; alert after N consecutive failures.
- Auth-expiry detection from `AuthState`; prompt for re-auth.
- Schema-change detection: hash the shape of a source's output; flag drift.
- Source dashboard: health, last successful fetch, next scheduled fetch.

### 8.16 Multi-Device, API, and Sync

_Gap 11. New. From: shelf (AutoMerge / Tailscale)._

- **Sync model: CRDT peer sync (Decision 1).** Every device holds a full replica
  of the CRDT knowledge base and merges changes automatically; no device is a
  required hub for reads/writes. Replicas gossip over Tailscale when reachable and
  work fully offline otherwise.
- **Compute peer.** Enrichment sidecars (Ollama, Playwright, DICTATOR) realistically
  run only on the Mac, so the Mac is the compute peer: it claims un-enriched
  artifacts, runs the pipeline, and its results (summaries, vectors, links)
  replicate to all peers as ordinary synced data. Phones and tablets capture
  (PUSH) and review; they do not enrich.
- **What syncs vs not** is defined in 6.0: knowledge syncs; the job queue and
  derived indexes are local and rebuilt per device.
- HTTP API with token auth; responsive web UI for review/triage on phone/tablet.
- Push notifications (digest, high-relevance alerts, review reminders).
- Mobile capture via share-sheet -> ingest API (writes the local replica, syncs
  opportunistically).
- **CRDT engine** (implementation-level, within Decision 1): **cr-sqlite primary**
  — it turns the SQLite tables themselves into conflict-free replicated relations
  (`crsql_as_crr`), so "SQLite is the CRDT" and there is no dual store; it matches
  the SQLite-centric data model directly. **AutoMerge fallback** (documents +
  SQLite projection; shelf precedent) if cr-sqlite proves blocking. Deciding test
  at Phase 10: loading the native SQLite extension on every target (macOS/Go is
  trivial; iOS and any web replica are the risk — AutoMerge has a clean Swift/WASM
  story there). The Phase 0 base schema is identical for both engines.

### 8.17 Privacy and Access Control

_Implemented (Phase C read surface + Phase 18 audit log)._

- Four tiers: public, personal, sensitive, restricted; default per source
  (`KB_DEFAULT_PRIVACY`); per-artifact override. `PrivacyTier.Rank`/`AtMost`
  give the ordering; a schema CHECK constrains the stored values.
- LLM routing rule: sensitive/restricted -> local Ollama only, never cloud
  (the summarize stage skips them unless `KB_ALLOW_REMOTE_OLLAMA`).
- **Two principals, one read ceiling.** The owner (`KB_API_TOKEN`) has the
  `restricted` ceiling and sees everything. An optional lower-trust read token
  (`KB_READ_TOKEN`, ceiling `KB_READ_TIER` ∈ {public, personal, sensitive};
  `restricted` is rejected) reaches only the read routes and only artifacts at
  or below its ceiling.
- **Enforcement is a SQL filter, not post-load redaction.** Each read method
  takes an explicit `ceiling` parameter expanded by `kb.TierClause` into the
  query (`tier IN (...)`); owner = `1 = 1` (no filter), invalid = `1 = 0` (fail
  closed). Above-ceiling rows are never loaded. The `requireRead` middleware
  authenticates the principal and tags the request context with its ceiling;
  each handler reads it via `readCeiling(ctx)` and threads it into the store.
  Adding a new read route means mounting it under `requireRead` and passing the
  ceiling — the invariant the design rests on.
- **Existence hiding.** An above-ceiling artifact, entity, topic, or graph node
  returns 404, identical to a non-existent one — a lower-trust reader cannot
  distinguish "forbidden" from "absent." Entities/topics carry no tier of their
  own; their visibility is derived from in-ceiling artifact membership (an
  entity name is exposed only because an artifact the reader can already read
  mentions it). `/ask` and `/search` over a ceiling-filtered corpus remain a
  bounded existence oracle (empty answer) by construction; no id/title/body
  leaks.
- Export/render filter enforces tiers on every rendered artifact (pre-Phase-C).
- **Access audit log (Phase 18).** Every read that surfaces a sensitive or
  restricted artifact records one `audit_log` entry — actor (owner | read-token),
  action (the endpoint), artifact id, and tier — across `/search`, `/surface`,
  `/graph`, and `/ask`. The middleware tags the request with the actor; a
  best-effort write never fails the read it describes. Public/personal accesses
  are not logged (high-volume, low-signal). `audit_log` is local-only (never
  synced; no FK to artifacts, so a row survives the artifact's compaction). `GET
/audit` (owner-only — the log is itself sensitive) inspects it.
- _Deferred:_ audit-log retention/GC (the log grows unbounded); auditing
  list-only reads that expose titles without loading the full artifact
  (entities/backlinks); exporting the audit log. The cross-tier relevance-score
  influence (an engaged sensitive artifact shapes the score of public ones) is a
  known limitation — acceptable because the score is never serialized out to a
  read token.

---

## 9. Event Bus and Triggers

The core publishes events; subsystems subscribe. Inherited from `haus` (SSE) and
`shelf` (lifecycle triggers).

**Durability rule (load-bearing).** The bus is in-process, lossy (a slow
subscriber drops events), and non-durable (a process restart loses in-flight
events). It is therefore for **notifications only** — SSE, UI, and best-effort
observers. Anything that _must_ durably happen is a **transactionally-enqueued
job**, never a bus subscriber: the publishing code enqueues the job in the same
transaction as the write that triggers it (the transactional-outbox pattern). So
in the table below, "subscribers" like the enrichment runner and re-score jobs
are reached via the **queue** (the writer enqueues), with the bus carrying only
the human/UI-facing notification and a wake-up hint to the runner. Wiring durable
work onto the bus would silently lose it on a drop or restart.

| Event                 | Published when             | Key subscribers                  |
| --------------------- | -------------------------- | -------------------------------- |
| `artifact.ingested`   | artifact/encounter written | enrichment runner                |
| `artifact.ready`      | pipeline complete          | search index, surfacing          |
| `engagement.recorded` | user interacts             | feedback loop, decay             |
| `profile.updated`     | profile-learning runs      | re-score jobs, fetch prioritizer |
| `source.failed`       | N consecutive failures     | health monitor, notifier         |
| `conflict.detected`   | contradiction found        | conflicts queue, UI              |
| `review.due`          | spaced-repetition timer    | surfacing, mobile push           |
| `digest.ready`        | digest rendered            | notifier, web UI                 |
| `activity.observed`   | tldw/active-repo signal    | context-aware suggestions        |

---

## 10. Technology Stack

| Layer                                    | Choice                                      | Rationale (report ref)                             |
| ---------------------------------------- | ------------------------------------------- | -------------------------------------------------- |
| Core daemon                              | Go                                          | rememberer proves the stack (Rec 1); single binary |
| Database                                 | SQLite (WAL)                                | local-first, embedded                              |
| Vectors                                  | sqlite-vec                                  | proven in rememberer                               |
| Fuzzy/title search                       | SQLite FTS5                                 | braindrain dual-search                             |
| LLM / embeddings / vision                | Ollama (local)                              | privacy; rememberer, tldr-web, local-tldr          |
| Web extraction                           | Playwright (Node sidecar)                   | modern JS pages (Rec 4)                            |
| Transcription                            | DICTATOR                                    | external (Gap 13)                                  |
| Rendering                                | pandoc + patat + ffmpeg                     | Output layer                                       |
| Scheduling                               | launchd                                     | arc-tabs-logger pattern                            |
| Multi-device transport                   | HTTP + SSE over Tailscale                   | haus; shelf sync                                   |
| Sync / replication                       | CRDT: AutoMerge or cr-sqlite over Tailscale | Decision 1; shelf precedent                        |
| Browser capture                          | MV3 / WebExtension                          | rememberer, local-tldr                             |
| Cloud LLM (optional, non-sensitive only) | Bedrock/Anthropic                           | privacy-gated by tier                              |

---

## 11. Configuration Model

- System config: database path, sidecar endpoints, default privacy tier, cloud-
  LLM enable flag.
- Per-source config rows in `sources` (Section 6.1): connector, schedule policy,
  auth, default privacy, storage budget.
- Human-editable config favored where practical (the `gg`/`dagger` TOML lineage)
  for source definitions, kept in sync with the `sources` table.

---

## 12. Build Sequence (Dependency-Ordered)

Phases are ordered by **technical dependency only** — what must exist before the
next thing can function. All phases are in scope; none is optional. Ordering does
not reflect effort or value.

**Phase 0 — Spine.** Core daemon skeleton: SQLite schema (Section 6), job queue +
DAG runner, SSE event bus, HTTP API skeleton, scheduler, config. Everything
depends on this.

**Phase 1 — One artifact, correctly.** Ingest API, artifact/encounter model,
canonicalization + dedup (8.2). Validates the keystone before anything builds on
it.

**Phase 2 — Enrichment core.** Content-type router + Playwright fetcher (8.3),
Ollama summarize/tags/critical-analysis/embed, relevance scoring against a static
profile (8.5, 8.9). Raw artifacts become enriched, searchable ones.

**Phase 3 — Retrieval.** Hybrid search then RAG (8.8). The store becomes useful.

**Phase 4 — Sources at scale.** Formalize `Source`; port arc-tabs-logger,
rememberer extension, shelf importers; add RSS/social/YouTube/podcast; scheduler
policies (unfurl); source health monitoring (8.1, 8.15).

**Phase 5 — Knowledge graph.** Entity extraction, link discovery, backlinks,
topic clustering, graph query + mindmapper view (8.7).

**Phase 6 — The loop.** Engagement capture, profile learning, relevance
re-scoring, decay, lifecycle/GC (8.10, 8.12). The system stops being a funnel.

**Phase 7 — Surfacing.** Digest, spaced repetition, context-aware suggestions,
serendipity, trend detection, weekly review (8.11).

**Phase 8 — Transcription + ambient.** DICTATOR integration, tldw activity
pipeline (adaptive cadence: perceptual-hash dedup before the vision-LLM,
Decision 6), camera-roll/OCR capture, diarization (8.4, ambient parts of 8.1).

**Phase 9 — Output/render.** pandoc/patat/Eisvogel/HTML/DOCX/EPUB + radio_hn
audio; wire digests and RAG answers to render targets (8.14).

**Phase 10 — Multi-device.** API hardening, web UI, mobile capture, push, and
CRDT peer sync: pick the CRDT engine (AutoMerge vs cr-sqlite), make the SQLite
schema a projection of the CRDT layer, designate the Mac as compute peer, and
rebuild derived indexes locally (8.16, 6.0).

**Phase 11 — Advanced enrichment.** Conflict/claim detection, source credibility
(8.13).

Dependency rationale: you cannot index before you store (0->2), cannot RAG before
you embed (2->3), cannot learn from engagement before you surface things to
engage with (6->7 read together), cannot transcribe-then-enrich before the
enrichment pipeline exists (2->8).

---

## 13. Migration Plan

- **notes/** (100+ Markdown files): import as `note` artifacts; bodies are
  already Markdown, so they flow straight into enrichment. The migration target
  named in the report.
- **rememberer** data: import highlights as artifacts + encounters with context;
  reuse its embeddings if the embed model matches, else re-embed.
- **braindrain** tree: import nodes as artifacts; parent refs become
  `references`/`derived_from` links.
- **shelf** imported objects: map each importer's records to artifacts via the
  new `Source` connectors.
- **arc-tabs-logger** log: backfill as webpage encounters (dedup will collapse
  repeats into canonical artifacts).
- Existing tools become connectors or renderers, not standalone apps (the report's
  core anti-pattern: 22 separate projects).

---

## 14. Design Decisions (Resolved)

Decisions 1, 2, 3, and 5 were chosen explicitly; 4 and 6 took the recommended
default. Each notes the phase it governs and its downstream impact.

| #   | Decision                 | Chosen                                       | Governs  | Downstream impact                                                                                                                                                                                            |
| --- | ------------------------ | -------------------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 1   | Sync model               | **CRDT peer sync** (AutoMerge or cr-sqlite)  | Phase 10 | Data model split into synced/local classes (6.0); SQLite becomes a CRDT projection (8.6); Mac is compute peer (8.16); client-generated ULIDs                                                                 |
| 2   | Embedding model          | **Local-only** (Ollama, nomic-embed 768-dim) | Phase 2  | One vector space across all tiers; private by construction; archive re-embeddable via versioned enrichment (6.5)                                                                                             |
| 3   | RAG chunking             | **Structural + fixed-size fallback**         | Phase 3  | Splits on Markdown blocks (free from Principle 2); oversized blocks sub-split (6.5, 8.8)                                                                                                                     |
| 4   | Job queue location       | **In-SQLite** (default)                      | Phase 0  | Transactional enqueue (write artifact + enqueue in one tx); queue is local-only, not synced (6.0, 6.11)                                                                                                      |
| 5   | Conflict scope           | **Within-topic / linked-cluster**            | Phase 11 | Compare claims only within shared topic / N hops, reusing the search vector pre-filter (8.13)                                                                                                                |
| 6   | tldw cadence             | **Adaptive** (default)                       | Phase 8  | Sample faster on window change; vision-LLM only on perceptual-hash-changed frames (8.1, Phase 8)                                                                                                             |
| 7   | `updated_at` maintenance | **Repository-managed, no DB trigger**        | Phase 0  | A trigger writing `datetime('now')` self-fires and, under cr-sqlite, overwrites a merged row's timestamp with local wall-clock — non-deterministic across replicas (6.0). Repos set `updated_at` explicitly. |
| 8   | Transaction locking      | **`_txlock=immediate`** in the DSN           | Phase 0  | `BEGIN IMMEDIATE` takes the write lock up front, so the single-writer pool + busy-timeout serialize writers instead of racing into `SQLITE_BUSY` (8.6)                                                       |

### Decision 1 consequences (CRDT peer sync)

This is the load-bearing choice. It changes the system from "one Mac with remote
clients" to "N equal replicas plus one compute peer," which surfaces in:

- **6.0 Sync classification** — which tables replicate (knowledge), which are
  local and rebuilt (vector / FTS indexes), which are device-specific (jobs,
  audit log).
- **8.6 Storage** — SQLite is a materialized projection of the CRDT layer.
- **8.16 Multi-device** — peer gossip over Tailscale; the Mac as compute peer;
  offline-first capture on every device.
- **Identifiers** — ULIDs minted client-side so any replica can create records
  offline without coordination.
- **CRDT engine sub-choice** — leaning **cr-sqlite** (in-SQLite table CRDT, no
  dual store) over AutoMerge (documents + SQLite projection); deciding test at
  Phase 10 is native-extension loading on iOS/web. Strategic direction (CRDT peer
  sync) is fixed; the Phase 0 schema is engine-agnostic, so this does not gate
  Phases 0–9.

---

## 15. Traceability

### 15.1 Report Gaps -> Subsystem

| Gap                                | Subsystem                      |
| ---------------------------------- | ------------------------------ |
| 1 Uncovered data sources           | 8.1 Connectors                 |
| 2 Dedup / canonical resolution     | 8.2 Ingestion/Dedup            |
| 3 Knowledge graph / linking        | 8.7 Graph                      |
| 4 Decay / resurfacing / spaced rep | 8.11 Surfacing, 8.12 Lifecycle |
| 5 Proactive surfacing              | 8.11 Surfacing                 |
| 6 Conversational query (RAG)       | 8.8 Retrieval                  |
| 7 Conflict detection               | 8.13 Conflicts                 |
| 8 Lifecycle / GC                   | 8.12 Lifecycle                 |
| 9 Source health monitoring         | 8.15 Health                    |
| 10 User feedback loop              | 8.10 Feedback                  |
| 11 Multi-device access             | 8.16 Multi-device              |
| 12 Privacy boundaries              | 8.17 Privacy                   |
| 13 Transcription (DICTATOR)        | 8.4 Transcription              |
| 14 Feedback cycle (architecture)   | Section 5, 8.9, 8.10           |

### 15.2 Existing Projects -> Role

| Project                           | Role in KB                                                  |
| --------------------------------- | ----------------------------------------------------------- |
| rememberer                        | foundation stack; PUSH connector; semantic search           |
| shelf                             | connector architecture; importer catalog; ACL/privacy basis |
| unfurl                            | scheduling/policy layer                                     |
| tldr-web                          | Playwright fetcher; structured LLM output                   |
| bookmarks-manager                 | content-type router; graceful degradation                   |
| digidex / summarize-url.sh        | enrichment + relevance model; critical analysis             |
| braindrain                        | dual search; tree links; soft-delete                        |
| haus                              | SSE event bus; triggers; object store ideas                 |
| tldw                              | ambient activity connector                                  |
| mindmapper                        | knowledge-graph visual view                                 |
| radio_hn                          | audio renderer                                              |
| patat / Eisvogel / Bootstrap tmpl | document/slide renderers                                    |
| arc-tabs-logger                   | launchd scheduling pattern; tabs connector                  |
| gg / notes / dagger               | Markdown-at-rest; human-editable config; migration target   |
| lineout                           | outline/focus browsing UI                                   |
| local-tldr                        | client-side PUSH capture                                    |
| rag-codebase                      | code-artifact enrichment (AST + annotation)                 |
| DICTATOR                          | transcription backbone (external)                           |
