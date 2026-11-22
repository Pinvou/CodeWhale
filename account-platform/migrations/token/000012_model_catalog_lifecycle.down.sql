ALTER TABLE usage_requests DROP CONSTRAINT usage_requests_model_version_snapshot_complete;
ALTER TABLE usage_requests
    DROP COLUMN model_snapshot,
    DROP COLUMN route_generation,
    DROP COLUMN protocol_definition_id,
    DROP COLUMN model_capability_version_id,
    DROP COLUMN catalog_version,
    DROP COLUMN model_id;
ALTER TABLE routing_policies DROP COLUMN route_generation;
DROP TABLE model_catalog_audit;
DROP TABLE model_catalog_commands;
DROP TABLE model_catalog_drafts;
DROP TABLE model_catalog_entries;
DROP TABLE model_catalog_versions;
DROP TABLE model_capability_versions;
DROP TABLE model_identities;
DROP FUNCTION reject_model_catalog_history_mutation();
