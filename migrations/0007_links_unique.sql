-- Dedup graph edges: a given (src, dst, relation) is recorded once. The graph
-- builder re-runs idempotently as artifacts re-enrich, so this lets it INSERT OR
-- IGNORE without multiplying edges. (links carries no such constraint in the base
-- schema; the index adds it without a table rebuild.)
CREATE UNIQUE INDEX idx_links_unique ON links(src_id, dst_id, relation);
