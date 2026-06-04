-- Per-peer changeset watermarks for multi-device sync (§8.16, Stage 3b). The puller
-- records the highest db_version it has pulled from each peer so the next pull only
-- transfers newer changes. Device-local (never synced, never a CRR): each device
-- tracks its own peers independently. Re-pulling from a lower watermark is always
-- safe (cr-sqlite's merge is idempotent), so a lost or reset watermark only costs
-- bandwidth.
CREATE TABLE sync_state (
  peer         TEXT NOT NULL PRIMARY KEY, -- peer base URL
  last_version INTEGER NOT NULL DEFAULT 0,
  updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
