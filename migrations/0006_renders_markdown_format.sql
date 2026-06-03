-- Extend the renders.format whitelist with 'markdown': the dependency-free output
-- the render layer (§8.14) produces directly from assembled artifact bodies, with
-- no pandoc/patat subprocess. SQLite cannot alter a CHECK constraint in place, so
-- the table is rebuilt. renders carries no foreign keys in either direction
-- (nothing references it; it references nothing), so a straight copy is complete
-- and safe inside the migration transaction.
ALTER TABLE renders RENAME TO renders_old;

CREATE TABLE renders (
  id           TEXT PRIMARY KEY,
  format       TEXT NOT NULL
                 CHECK (format IN ('markdown','patat','pdf','html','docx','epub','audio')),
  source_query TEXT,
  artifact_ids TEXT,                             -- JSON array
  output_path  TEXT,
  privacy_max  TEXT NOT NULL                     -- highest tier included (export filter)
                 CHECK (privacy_max IN ('public','personal','sensitive','restricted')),
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO renders(id, format, source_query, artifact_ids, output_path, privacy_max, created_at)
  SELECT id, format, source_query, artifact_ids, output_path, privacy_max, created_at
  FROM renders_old;

DROP TABLE renders_old;
