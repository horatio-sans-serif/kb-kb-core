-- Make every synced table acceptable to cr-sqlite's crsql_as_crr (§8.16 / §6.0).
-- An empirical probe against the real extension found four constraints CRRs must
-- satisfy that the original schema violated:
--   (A) a NON-NULL primary key — SQLite's bare `TEXT PRIMARY KEY` is nullable;
--   (B) no non-primary-key UNIQUE indexes — uniqueness can't hold across replicas
--       (entities' UNIQUE(kind,name) is the last one, after 0011/0012);
--   (C) no CHECKED foreign keys — cr-sqlite inspects the schema (not the runtime
--       pragma): a replica can receive a child before its parent, so the FK must
--       not be enforced. Columns stay; only the REFERENCES constraint is dropped;
--   (D) every NON-NULL column (except the primary key) must have a DEFAULT — so a
--       replica on a different schema version can insert a row without it. Required
--       columns the application always supplies get a formal default ('' for text,
--       0 for numbers, a valid enum member for CHECK columns); it is never used in
--       practice because every real write and every replicated change carries the
--       value.
--
-- SQLite cannot ALTER a column to add NOT NULL, a DEFAULT, drop a UNIQUE, or drop a
-- FK, so each synced table is rebuilt (create -> copy -> drop -> rename). Local-only
-- tables (jobs, audit_log, schema_migrations, the FTS shadow tables) are NOT rebuilt
-- and keep their foreign keys to artifacts. Referential integrity is preserved by the
-- application (parent-before-child writes within a transaction) and the reconcile
-- sweep.
--
-- The migration runner applies this file with foreign-key enforcement disabled on the
-- connection (PRAGMA foreign_keys is a no-op inside a transaction, so it cannot be set
-- here). That is required: DROP TABLE artifacts, while jobs/audit_log still reference
-- it, would otherwise fail at COMMIT — defer_foreign_keys is NOT sufficient, because
-- the parent DROP raises a deferred violation the later re-create does not clear.
-- Tables are still rebuilt children-before-parents for clarity.

-- conflicts (was REFERENCES claims)
CREATE TABLE conflicts__crr (
  id         TEXT NOT NULL PRIMARY KEY,
  claim_a    TEXT NOT NULL DEFAULT '',
  claim_b    TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','dismissed')),
  rationale  TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO conflicts__crr SELECT id, claim_a, claim_b, status, rationale, created_at FROM conflicts;
DROP TABLE conflicts;
ALTER TABLE conflicts__crr RENAME TO conflicts;
CREATE INDEX idx_conflicts_pair ON conflicts(claim_a, claim_b);

-- claims (was REFERENCES artifacts)
CREATE TABLE claims__crr (
  id          TEXT NOT NULL PRIMARY KEY,
  artifact_id TEXT NOT NULL DEFAULT '',
  statement   TEXT NOT NULL DEFAULT '',
  embedding   TEXT,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO claims__crr SELECT id, artifact_id, statement, embedding, created_at FROM claims;
DROP TABLE claims;
ALTER TABLE claims__crr RENAME TO claims;
CREATE INDEX idx_claims_artifact ON claims(artifact_id);
CREATE INDEX idx_claims_artifact_statement ON claims(artifact_id, statement);

-- encounters (was REFERENCES artifacts, sources)
CREATE TABLE encounters__crr (
  id          TEXT NOT NULL PRIMARY KEY,
  artifact_id TEXT NOT NULL DEFAULT '',
  source_id   TEXT NOT NULL DEFAULT '',
  raw_url     TEXT,
  context     TEXT,
  seen_at     TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO encounters__crr SELECT id, artifact_id, source_id, raw_url, context, seen_at, created_at FROM encounters;
DROP TABLE encounters;
ALTER TABLE encounters__crr RENAME TO encounters;
CREATE INDEX idx_encounters_artifact ON encounters(artifact_id);
CREATE INDEX idx_encounters_source   ON encounters(source_id);

-- enrichments (was REFERENCES artifacts)
CREATE TABLE enrichments__crr (
  id             TEXT NOT NULL PRIMARY KEY,
  artifact_id    TEXT NOT NULL DEFAULT '',
  kind           TEXT NOT NULL DEFAULT '',
  payload        TEXT NOT NULL DEFAULT '',
  model          TEXT NOT NULL DEFAULT '',
  prompt_version TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO enrichments__crr SELECT id, artifact_id, kind, payload, model, prompt_version, created_at FROM enrichments;
DROP TABLE enrichments;
ALTER TABLE enrichments__crr RENAME TO enrichments;
CREATE INDEX idx_enrichments_artifact_kind ON enrichments(artifact_id, kind);

-- artifact_entities (was REFERENCES artifacts, entities; composite PK already NOT NULL)
CREATE TABLE artifact_entities__crr (
  artifact_id TEXT NOT NULL,
  entity_id   TEXT NOT NULL,
  PRIMARY KEY (artifact_id, entity_id)
);
INSERT INTO artifact_entities__crr SELECT artifact_id, entity_id FROM artifact_entities;
DROP TABLE artifact_entities;
ALTER TABLE artifact_entities__crr RENAME TO artifact_entities;
CREATE INDEX idx_artifact_entities_entity ON artifact_entities(entity_id);

-- artifact_topics (was REFERENCES artifacts, topics)
CREATE TABLE artifact_topics__crr (
  artifact_id TEXT NOT NULL,
  topic_id    TEXT NOT NULL,
  score       REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (artifact_id, topic_id)
);
INSERT INTO artifact_topics__crr SELECT artifact_id, topic_id, score FROM artifact_topics;
DROP TABLE artifact_topics;
ALTER TABLE artifact_topics__crr RENAME TO artifact_topics;

-- links (was REFERENCES artifacts x2)
CREATE TABLE links__crr (
  id         TEXT NOT NULL PRIMARY KEY,
  src_id     TEXT NOT NULL DEFAULT '',
  dst_id     TEXT NOT NULL DEFAULT '',
  relation   TEXT NOT NULL DEFAULT 'references' CHECK (relation IN (
               'references','shares_entity','shares_topic','same_as','derived_from')),
  weight     REAL NOT NULL DEFAULT 1.0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO links__crr SELECT id, src_id, dst_id, relation, weight, created_at FROM links;
DROP TABLE links;
ALTER TABLE links__crr RENAME TO links;
CREATE INDEX idx_links_src ON links(src_id);
CREATE INDEX idx_links_dst ON links(dst_id);
CREATE INDEX idx_links_unique ON links(src_id, dst_id, relation);

-- engagements (was REFERENCES artifacts)
CREATE TABLE engagements__crr (
  id          TEXT NOT NULL PRIMARY KEY,
  artifact_id TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL DEFAULT 'view' CHECK (kind IN (
                'view','dwell','share','annotate','act',
                'thumbs_up','thumbs_down','dismiss','more_like','less_like')),
  value       REAL,
  note        TEXT,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO engagements__crr SELECT id, artifact_id, kind, value, note, created_at FROM engagements;
DROP TABLE engagements;
ALTER TABLE engagements__crr RENAME TO engagements;
CREATE INDEX idx_engagements_artifact ON engagements(artifact_id);

-- review_schedule (was artifact_id PRIMARY KEY REFERENCES artifacts)
CREATE TABLE review_schedule__crr (
  artifact_id    TEXT NOT NULL PRIMARY KEY,
  next_review_at TEXT NOT NULL DEFAULT '',
  interval_days  REAL NOT NULL DEFAULT 1,
  ease           REAL NOT NULL DEFAULT 2.5,
  reviews        INTEGER NOT NULL DEFAULT 0
);
INSERT INTO review_schedule__crr SELECT artifact_id, next_review_at, interval_days, ease, reviews FROM review_schedule;
DROP TABLE review_schedule;
ALTER TABLE review_schedule__crr RENAME TO review_schedule;
CREATE INDEX idx_review_due ON review_schedule(next_review_at);

-- profile_history (was REFERENCES profile_interests)
CREATE TABLE profile_history__crr (
  id          TEXT NOT NULL PRIMARY KEY,
  interest_id TEXT NOT NULL DEFAULT '',
  weight      REAL NOT NULL DEFAULT 0,
  recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO profile_history__crr SELECT id, interest_id, weight, recorded_at FROM profile_history;
DROP TABLE profile_history;
ALTER TABLE profile_history__crr RENAME TO profile_history;

-- renders (no FK; nullable PK + missing defaults)
CREATE TABLE renders__crr (
  id           TEXT NOT NULL PRIMARY KEY,
  format       TEXT NOT NULL DEFAULT 'markdown'
                 CHECK (format IN ('markdown','patat','pdf','html','docx','epub','audio')),
  source_query TEXT,
  artifact_ids TEXT,
  output_path  TEXT,
  -- Class-D needs a default; pick the MOST restrictive tier (fail-closed) so a write
  -- that omits the export ceiling never silently widens it. Every real writer sets it.
  privacy_max  TEXT NOT NULL DEFAULT 'restricted'
                 CHECK (privacy_max IN ('public','personal','sensitive','restricted')),
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO renders__crr SELECT id, format, source_query, artifact_ids, output_path, privacy_max, created_at FROM renders;
DROP TABLE renders;
ALTER TABLE renders__crr RENAME TO renders;

-- entities (drop UNIQUE(kind,name) — class B; nullable PK — class A)
CREATE TABLE entities__crr (
  id      TEXT NOT NULL PRIMARY KEY,
  kind    TEXT NOT NULL DEFAULT 'concept' CHECK (kind IN ('person','org','technology','concept','place')),
  name    TEXT NOT NULL DEFAULT '',
  aliases TEXT
);
INSERT INTO entities__crr SELECT id, kind, name, aliases FROM entities;
DROP TABLE entities;
ALTER TABLE entities__crr RENAME TO entities;
-- Non-unique lookup index for the find-then-insert dedup by (kind, name).
CREATE INDEX idx_entities_kind_name ON entities(kind, name);

-- topics (nullable PK + missing default)
CREATE TABLE topics__crr (
  id       TEXT NOT NULL PRIMARY KEY,
  label    TEXT NOT NULL DEFAULT '',
  centroid TEXT
);
INSERT INTO topics__crr SELECT id, label, centroid FROM topics;
DROP TABLE topics;
ALTER TABLE topics__crr RENAME TO topics;

-- profile_interests (nullable PK + missing defaults)
CREATE TABLE profile_interests__crr (
  id         TEXT NOT NULL PRIMARY KEY,
  label      TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL DEFAULT 'interest' CHECK (kind IN ('interest','project','employment','hobby')),
  weight     REAL NOT NULL DEFAULT 1.0,
  embedding  TEXT,
  active     INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO profile_interests__crr SELECT id, label, kind, weight, embedding, active, updated_at FROM profile_interests;
DROP TABLE profile_interests;
ALTER TABLE profile_interests__crr RENAME TO profile_interests;

-- sources (nullable PK + missing defaults; includes the 0005 cursor/last_fetched_at/health columns)
CREATE TABLE sources__crr (
  id              TEXT NOT NULL PRIMARY KEY,
  kind            TEXT NOT NULL DEFAULT 'PULL' CHECK (kind IN ('PULL','PUSH','AMBIENT')),
  connector       TEXT NOT NULL DEFAULT '',
  config          TEXT NOT NULL DEFAULT '{}',
  schedule        TEXT,
  auth_state      TEXT,
  default_privacy TEXT NOT NULL DEFAULT 'personal'
                    CHECK (default_privacy IN ('public','personal','sensitive','restricted')),
  storage_budget  INTEGER,
  source_weight   REAL NOT NULL DEFAULT 1.0,
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  cursor          TEXT,
  last_fetched_at TEXT,
  health          TEXT
);
INSERT INTO sources__crr SELECT id, kind, connector, config, schedule, auth_state, default_privacy,
  storage_budget, source_weight, enabled, created_at, cursor, last_fetched_at, health FROM sources;
DROP TABLE sources;
ALTER TABLE sources__crr RENAME TO sources;

-- artifacts (nullable PK + missing defaults; jobs/audit_log keep their FK to it)
CREATE TABLE artifacts__crr (
  id              TEXT NOT NULL PRIMARY KEY,
  type            TEXT NOT NULL DEFAULT 'note' CHECK (type IN (
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
  -- The '' default is a class-D formality: every insert (store.insert) and every
  -- replicated change carries a real TimeFormat timestamp, so a row never actually
  -- takes ''. (An '' would sort before all real timestamps, so do not start relying
  -- on the default — keep both columns always set at the write path.)
  first_seen_at   TEXT NOT NULL DEFAULT '',
  last_seen_at    TEXT NOT NULL DEFAULT '',
  last_engaged_at TEXT,
  deleted_at      TEXT,
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO artifacts__crr SELECT id, type, canonical_url, content_hash, title, body_markdown,
  privacy_tier, lifecycle_tier, status, relevance_score, first_seen_at, last_seen_at, last_engaged_at,
  deleted_at, created_at, updated_at FROM artifacts;
DROP TABLE artifacts;
ALTER TABLE artifacts__crr RENAME TO artifacts;
CREATE INDEX idx_artifacts_canonical     ON artifacts(canonical_url);
CREATE INDEX idx_artifacts_content_hash  ON artifacts(content_hash);
CREATE INDEX idx_artifacts_lifecycle     ON artifacts(lifecycle_tier, relevance_score DESC);
