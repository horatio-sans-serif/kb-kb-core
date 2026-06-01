-- Local-only full-text index over artifact title/body (design §8.8 search;
-- §6.0 local indexes). It is rebuilt from the synced `artifacts` table per device
-- and is NOT itself synced — when cr-sqlite lands (Phase 10) this table is
-- excluded from the CRR set.
--
-- Standalone (not external-content) so it does not depend on artifacts' rowid,
-- which is unstable across vacuum. The application maintains it transactionally on
-- artifact writes (ArtifactRepo.reindexFTS) rather than via DB triggers: §6.0
-- keeps triggers off the soon-to-be-CRR artifacts table so cr-sqlite's own
-- triggers are not doubled. Soft-deleted artifacts are excluded at query time
-- (the search join filters deleted_at IS NULL), so delete-sync is unnecessary.
--
-- Requires the sqlite_fts5 build tag (see Makefile / CI); without it this
-- statement fails with "no such module: fts5".
CREATE VIRTUAL TABLE artifacts_fts USING fts5(
  artifact_id UNINDEXED,
  title,
  body_markdown,
  tokenize = 'unicode61 remove_diacritics 2'
);

-- Backfill any artifacts that predate this index (no-op on a fresh database).
INSERT INTO artifacts_fts(artifact_id, title, body_markdown)
SELECT id, title, body_markdown FROM artifacts WHERE deleted_at IS NULL;
