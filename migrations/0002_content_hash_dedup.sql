-- Phase 1: content-hash dedup (§8.2). Replace the non-unique content_hash index
-- with a partial unique one scoped to live rows, so UpsertByContentHash dedups
-- atomically via ON CONFLICT, mirroring the canonical_url path. URL artifacts
-- carry a NULL content_hash until fetched (Phase 2), so they never collide here.
DROP INDEX IF EXISTS idx_artifacts_hash;

CREATE UNIQUE INDEX idx_artifacts_content_hash ON artifacts(content_hash)
  WHERE content_hash IS NOT NULL AND deleted_at IS NULL;
