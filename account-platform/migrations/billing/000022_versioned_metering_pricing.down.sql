DROP TABLE pricing_operation_commands;
DROP FUNCTION reject_pricing_operation_command_mutation();
DROP TRIGGER formula_binding_immutable ON billing_protocol_formula_bindings;
DROP FUNCTION reject_formula_binding_mutation();
DROP TRIGGER pricing_version_unit_published_immutable ON pricing_version_unit_configs;
DROP FUNCTION reject_published_price_unit_mutation();
DROP TRIGGER pricing_version_published_immutable ON pricing_versions;
DROP FUNCTION reject_published_price_mutation();
DROP TRIGGER metering_unit_immutable_update ON metering_unit_definitions;
DROP FUNCTION reject_immutable_metering_unit();
DROP INDEX pricing_blocks_match_idx;
DROP INDEX pricing_versions_match_idx;
ALTER TABLE usage_holds DROP COLUMN request_snapshot,DROP COLUMN pricing_snapshot,DROP COLUMN formula_version_id,DROP COLUMN protocol_code,DROP COLUMN model_id;
ALTER TABLE usage_holds DROP CONSTRAINT usage_holds_max_microcredits_check;
ALTER TABLE usage_holds ADD CONSTRAINT usage_holds_max_microcredits_check CHECK (max_microcredits > 0);
DROP TABLE pricing_audits;
DROP TABLE pricing_blocks;
DROP TABLE pricing_lifecycle_commands;
DROP TABLE pricing_publish_commands;
DROP TABLE pricing_space_versions;
DROP TABLE pricing_version_unit_configs;
ALTER TABLE pricing_versions DROP CONSTRAINT pricing_versions_lifecycle_check,DROP CONSTRAINT pricing_versions_scope_check,
    DROP COLUMN lifecycle_reason,DROP COLUMN publish_idempotency_key,DROP COLUMN request_fingerprint,DROP COLUMN created_by,
    DROP COLUMN ended_at,DROP COLUMN cancelled_at,DROP COLUMN published_at,DROP COLUMN space_etag,DROP COLUMN version,
    DROP COLUMN lifecycle_status,DROP COLUMN credit_unit,DROP COLUMN formula_version_id,DROP COLUMN protocol_code,DROP COLUMN scope_id,DROP COLUMN scope_type;
CREATE UNIQUE INDEX pricing_versions_active_model_idx ON pricing_versions (model) WHERE effective_to IS NULL;
DROP TABLE billing_protocol_formula_bindings;
DROP TABLE metering_unit_definitions;
