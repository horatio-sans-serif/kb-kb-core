-- cr-sqlite (§8.16) forbids non-primary-key unique indexes on replicated tables:
-- uniqueness can't be enforced across offline replicas. Following the artifact move
-- in 0011, links/claims/conflicts dedup moves from these unique indexes to
-- application-level find-then-insert (same-replica) plus the reconcile sweep
-- (cross-replica). Keep non-unique replacements so the dedup-key lookups stay fast.
--
-- entities' UNIQUE(kind, name) is a table-level constraint (an auto-index), not a
-- standalone index, so it is removed by the table rebuild in 0013, not here.
DROP INDEX IF EXISTS idx_links_unique;
DROP INDEX IF EXISTS idx_claims_artifact_statement;
DROP INDEX IF EXISTS idx_conflicts_pair;
CREATE INDEX IF NOT EXISTS idx_links_unique ON links(src_id, dst_id, relation);
CREATE INDEX IF NOT EXISTS idx_claims_artifact_statement ON claims(artifact_id, statement);
CREATE INDEX IF NOT EXISTS idx_conflicts_pair ON conflicts(claim_a, claim_b);
