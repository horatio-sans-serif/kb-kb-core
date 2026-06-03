-- PULL fetch state for the source scheduler (§8.1 / §8.15). cursor is the opaque
-- incremental position a connector returns and is fed back on the next Fetch;
-- last_fetched_at drives the refresh schedule (due when last_fetched_at + ttl <=
-- now); health is the JSON snapshot from the last poll (for the health endpoint).
-- All nullable: a source that has never been polled has none set.
ALTER TABLE sources ADD COLUMN cursor TEXT;
ALTER TABLE sources ADD COLUMN last_fetched_at TEXT;
ALTER TABLE sources ADD COLUMN health TEXT;
