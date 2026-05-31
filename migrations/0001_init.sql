-- KB Core — initial schema (Phase 0)
-- See UNIFIED_SYSTEM_DESIGN_PLAN.md Section 6 for the data model and Section 6.0
-- for the sync classification this schema implements.
--
-- Engine-agnostic base schema. Both CRDT engine candidates run over these exact
-- tables:
--   * cr-sqlite (primary): call crsql_as_crr('<table>') on the SYNCED tables at
--     Phase 10 to promote them to conflict-free replicated relations. No schema
--     change required.
--   * AutoMerge (fallback): these tables are the local projection of AutoMerge
--     documents; the projector writes here.
--
-- IDs are client-generated ULIDs (TEXT) so any replica can mint records offline.
-- Timestamps are ISO-8601 TEXT. created_at/updated_at use local wall-clock
-- (datetime('now')); these are audit timestamps, not logical merge clocks (see
-- the cr-sqlite readiness note in 6.0). JSON is stored as TEXT.
--
-- Connection PRAGMAs (journal_mode=WAL, foreign_keys=ON) are set by store.Open's
-- DSN, not here, so this file is pure schema and the runner can wrap it in one
-- transaction. The schema_migrations table and its version rows are owned by the
-- migration runner (store.Migrate); migration files must not touch it.

-- ============================================================================
-- SYNCED (CRDT source of truth) — replicated to every device. See 6.0.
-- ============================================================================

-- 6.1 Sources
CREATE TABLE sources (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL CHECK (kind IN ('PULL','PUSH','AMBIENT')),
  connector       TEXT NOT NULL,
  config          TEXT NOT NULL DEFAULT '{}',   -- JSON
  schedule        TEXT,                          -- JSON: {cron|ttl|rrule}
  auth_state      TEXT,                          -- JSON: token/oauth + expiry
  default_privacy TEXT NOT NULL DEFAULT 'personal'
                    CHECK (default_privacy IN ('public','personal','sensitive','restricted')),
  storage_budget  INTEGER,                       -- max artifacts retained (NULL = unbounded)
  source_weight   REAL NOT NULL DEFAULT 1.0,     -- adjusted by yield (feedback loop)
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- 6.2 Artifacts (canonical knowledge items)
CREATE TABLE artifacts (
  id              TEXT PRIMARY KEY,
  type            TEXT NOT NULL CHECK (type IN (
                    'webpage','pdf','video','audio','image','note',
                    'transcript','message','social_post','email','code','render')),
  canonical_url   TEXT,
  content_hash    TEXT,
  title           TEXT,
  body_markdown   TEXT,
  privacy_tier    TEXT NOT NULL DEFAULT 'personal'
                    CHECK (privacy_tier IN ('public','personal','sensitive','restricted')),
  lifecycle_tier  TEXT NOT NULL DEFAULT 'active'
                    CHECK (lifecycle_tier IN ('active','archive','purged')),
  status          TEXT NOT NULL DEFAULT 'ingested'
                    CHECK (status IN ('ingested','enriching','ready','failed')),
  relevance_score REAL,
  first_seen_at   TEXT NOT NULL,
  last_seen_at    TEXT NOT NULL,
  last_engaged_at TEXT,
  deleted_at      TEXT,                          -- soft-delete tombstone (CRDT-friendly)
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_artifacts_canonical ON artifacts(canonical_url) WHERE canonical_url IS NOT NULL;
CREATE INDEX idx_artifacts_hash       ON artifacts(content_hash);
-- Relevance is ranked within a lifecycle tier (the surfacing query, design 8.11),
-- so the composite below covers it. No standalone relevance_score index:
-- relevance is a hot-decay column, kept out of as many indexes as possible.
CREATE INDEX idx_artifacts_lifecycle  ON artifacts(lifecycle_tier, relevance_score DESC);

-- 6.3 Encounters (the dedup keystone — one artifact, many encounters)
CREATE TABLE encounters (
  id          TEXT PRIMARY KEY,
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  source_id   TEXT NOT NULL REFERENCES sources(id),
  raw_url     TEXT,
  context     TEXT,                              -- JSON: highlight, channel, sender, tweet id...
  seen_at     TEXT NOT NULL,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_encounters_artifact ON encounters(artifact_id);
CREATE INDEX idx_encounters_source   ON encounters(source_id);

-- 6.4 Enrichments (versioned; embedding vectors travel here as data so every
-- replica can rebuild its local ANN index — see 6.0 / 6.5)
CREATE TABLE enrichments (
  id             TEXT PRIMARY KEY,
  artifact_id    TEXT NOT NULL REFERENCES artifacts(id),
  kind           TEXT NOT NULL,                  -- summary|tags|critical_analysis|relevance|embedding|...
  payload        TEXT NOT NULL,                  -- JSON (schema-validated)
  model          TEXT NOT NULL,
  prompt_version TEXT NOT NULL,
  created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_enrichments_artifact_kind ON enrichments(artifact_id, kind);

-- 6.6 Knowledge graph
CREATE TABLE entities (
  id      TEXT PRIMARY KEY,
  kind    TEXT NOT NULL CHECK (kind IN ('person','org','technology','concept','place')),
  name    TEXT NOT NULL,
  aliases TEXT,                                  -- JSON array
  UNIQUE(kind, name)
);

CREATE TABLE artifact_entities (
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  entity_id   TEXT NOT NULL REFERENCES entities(id),
  PRIMARY KEY (artifact_id, entity_id)
);

CREATE TABLE links (
  id         TEXT PRIMARY KEY,
  src_id     TEXT NOT NULL REFERENCES artifacts(id),
  dst_id     TEXT NOT NULL REFERENCES artifacts(id),
  relation   TEXT NOT NULL CHECK (relation IN (
               'references','shares_entity','shares_topic','same_as','derived_from')),
  weight     REAL NOT NULL DEFAULT 1.0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_links_src ON links(src_id);
CREATE INDEX idx_links_dst ON links(dst_id);     -- backlinks

CREATE TABLE topics (
  id       TEXT PRIMARY KEY,
  label    TEXT NOT NULL,
  centroid TEXT                                  -- JSON: topic vector
);
CREATE TABLE artifact_topics (
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  topic_id    TEXT NOT NULL REFERENCES topics(id),
  score       REAL NOT NULL,
  PRIMARY KEY (artifact_id, topic_id)
);

-- 6.7 Living profile (versioned)
CREATE TABLE profile_interests (
  id         TEXT PRIMARY KEY,
  label      TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('interest','project','employment','hobby')),
  weight     REAL NOT NULL DEFAULT 1.0,          -- mutated by engagement
  embedding  TEXT,                               -- JSON: interest vector
  active     INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE profile_history (                   -- temporal: interest drift
  id          TEXT PRIMARY KEY,
  interest_id TEXT NOT NULL REFERENCES profile_interests(id),
  weight      REAL NOT NULL,
  recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- 6.8 Engagement (feedback signals)
CREATE TABLE engagements (
  id          TEXT PRIMARY KEY,
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  kind        TEXT NOT NULL CHECK (kind IN (
                'view','dwell','share','annotate','act',
                'thumbs_up','thumbs_down','dismiss','more_like','less_like')),
  value       REAL,                              -- e.g. dwell seconds
  note        TEXT,                              -- correction explanation
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_engagements_artifact ON engagements(artifact_id);

-- 6.9 Surfacing / spaced repetition
CREATE TABLE review_schedule (
  artifact_id    TEXT PRIMARY KEY REFERENCES artifacts(id),
  next_review_at TEXT NOT NULL,
  interval_days  REAL NOT NULL DEFAULT 1,        -- SM-2-style
  ease           REAL NOT NULL DEFAULT 2.5,
  reviews        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_review_due ON review_schedule(next_review_at);

-- 6.10 Conflict detection
CREATE TABLE claims (
  id          TEXT PRIMARY KEY,
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  statement   TEXT NOT NULL,
  embedding   TEXT,                              -- JSON: claim vector
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_claims_artifact ON claims(artifact_id);

CREATE TABLE conflicts (
  id         TEXT PRIMARY KEY,
  claim_a    TEXT NOT NULL REFERENCES claims(id),
  claim_b    TEXT NOT NULL REFERENCES claims(id),
  status     TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','dismissed')),
  rationale  TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- 6.11 Renders (generated output artifacts; metadata syncs, files are local)
CREATE TABLE renders (
  id           TEXT PRIMARY KEY,
  format       TEXT NOT NULL CHECK (format IN ('patat','pdf','html','docx','epub','audio')),
  source_query TEXT,
  artifact_ids TEXT,                             -- JSON array
  output_path  TEXT,
  privacy_max  TEXT NOT NULL                     -- highest tier included (export filter)
                 CHECK (privacy_max IN ('public','personal','sensitive','restricted')),
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ============================================================================
-- LOCAL-ONLY, device-specific — NOT synced. See 6.0.
-- ============================================================================

-- 6.11 Jobs (enrichment queue / DAG runner state)
CREATE TABLE jobs (
  id          TEXT PRIMARY KEY,
  type        TEXT NOT NULL,                     -- pipeline stage name
  artifact_id TEXT REFERENCES artifacts(id),
  depends_on  TEXT,                              -- JSON array of job ids
  payload     TEXT,                              -- JSON
  status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','running','done','failed')),
  attempts    INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT,
  run_after   TEXT,                              -- backoff scheduling
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_jobs_status ON jobs(status, run_after);

-- Audit log (records access on THIS device)
CREATE TABLE audit_log (
  id           TEXT PRIMARY KEY,
  actor        TEXT NOT NULL,                    -- device/api-key/local
  action       TEXT NOT NULL,
  artifact_id  TEXT,
  privacy_tier TEXT,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

-- ============================================================================
-- LOCAL-ONLY, rebuilt from synced data — derived indexes. See 6.0 / 6.5 / 8.6.
-- These are created on every device but populated locally; they do not sync.
-- The vec0 table is created at Phase 2 once the embed model/dim is fixed:
--
--   CREATE VIRTUAL TABLE embeddings USING vec0(
--     artifact_id TEXT, chunk_index INTEGER, embedding FLOAT[768]);
--
-- FTS5 over title + body for fuzzy/title search (braindrain dual-search) is
-- created at Phase 3 (search), alongside the vec0 index, so the Phase 0 core
-- schema carries no FTS5 / extension build dependency. It requires building the
-- driver with `-tags sqlite_fts5`:
--
--   CREATE VIRTUAL TABLE artifacts_fts USING fts5(artifact_id UNINDEXED, title, body);

-- No updated_at trigger: the repository layer (Stage 2) sets updated_at
-- explicitly. An AFTER UPDATE trigger writing datetime('now') would self-fire,
-- and under cr-sqlite it would overwrite a merged row's timestamp with local
-- wall-clock time — non-deterministic across replicas. See 6.0 readiness note.
