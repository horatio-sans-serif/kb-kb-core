-- cr-sqlite (§8.16) forbids non-primary-key unique indexes on replicated tables:
-- uniqueness can't be enforced across offline replicas (two devices can each save
-- the same URL while offline). Artifact dedup moves from the DB to application-
-- level reconciliation — the find-then-insert upsert prevents duplicates within a
-- replica, and the same_as merge sweep reconciles cross-replica ones after sync.
-- Drop the unique indexes; keep non-unique ones so the dedup-key lookup stays fast.
DROP INDEX IF EXISTS idx_artifacts_canonical;
DROP INDEX IF EXISTS idx_artifacts_content_hash;
CREATE INDEX IF NOT EXISTS idx_artifacts_canonical ON artifacts(canonical_url);
CREATE INDEX IF NOT EXISTS idx_artifacts_content_hash ON artifacts(content_hash);
