-- Indexes for the access audit log (§8.17, Phase 18). audit_log exists since 0001
-- (local-only, never synced, no FK to artifacts) but had no indexes — it was a
-- write-only stub until the read surface (Phase C) gave it something to record. The
-- query methods read newest-first and filter by artifact, so index both: created_at
-- (descending scans for the recent view) and artifact_id (the per-artifact history).
CREATE INDEX IF NOT EXISTS idx_audit_log_created ON audit_log(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_artifact ON audit_log(artifact_id);
