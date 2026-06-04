-- Index the entity->artifact direction. The artifact_entities PK is
-- (artifact_id, entity_id), so its index leads with artifact_id; queries that
-- filter/join on entity_id (the entity index and an entity's artifact list,
-- §8.7) would otherwise full-scan. Symmetric to idx_links_dst (the backlink
-- direction), which 0001 added for the same reason.
CREATE INDEX IF NOT EXISTS idx_artifact_entities_entity ON artifact_entities(entity_id);
