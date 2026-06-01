-- Make the hot Claim query indexable. Claim selects the oldest runnable pending
-- job (WHERE status='pending' ... ORDER BY created_at, id). A partial index on
-- pending rows in (created_at, id) order lets the planner walk it directly and
-- stop at the first runnable row, instead of materializing all pending rows into
-- a temp B-tree to sort. The index stays small because terminal (done/failed)
-- rows dominate over the daemon's lifetime and Purge removes them.
--
-- Drop idx_jobs_status: its (status, run_after) shape makes the planner prefer it
-- for the status= filter and then sort. Its only other users — ReclaimStuck and
-- Purge — run at startup over a small table, so a scan there is fine.
DROP INDEX IF EXISTS idx_jobs_status;

CREATE INDEX idx_jobs_claim ON jobs(created_at, id) WHERE status = 'pending';
