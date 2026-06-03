-- Dedup conflicts: a given pair of claims yields one conflict row. The detector
-- canonicalizes the pair (claim_a < claim_b) before inserting, so this unique
-- index lets it INSERT OR IGNORE without creating a second row when the same pair
-- is re-examined (or examined from the other claim's side). The base schema has no
-- such constraint; the index adds it without a table rebuild.
CREATE UNIQUE INDEX IF NOT EXISTS idx_conflicts_pair ON conflicts(claim_a, claim_b);
