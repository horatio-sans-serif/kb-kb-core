-- Idempotent claim extraction: a given (artifact_id, statement) is stored once.
-- The claims stage extracts once per artifact today, but this makes re-extraction
-- (a future "body changed" re-enrich) a no-op rather than a second generation of
-- duplicate claims — which would also defeat conflict dedup, since conflicts key on
-- claim ids. ClaimRepo.Create uses INSERT OR IGNORE against this index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_claims_artifact_statement ON claims(artifact_id, statement);
