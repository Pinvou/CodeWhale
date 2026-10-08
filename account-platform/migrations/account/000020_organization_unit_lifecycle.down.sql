DROP TRIGGER organization_unit_commands_immutable ON organization_unit_commands;
DROP FUNCTION protect_organization_unit_command();
DROP TRIGGER organization_units_lifecycle_guard ON organization_units;
DROP FUNCTION protect_organization_unit_lifecycle();
DROP TABLE organization_unit_governance_blockers;
DROP TABLE organization_unit_commands;
DROP INDEX organization_units_active_sibling_name_idx;
ALTER TABLE organization_units DROP CONSTRAINT organization_units_status_check;
ALTER TABLE organization_units ADD CONSTRAINT organization_units_status_check CHECK (status IN ('ACTIVE', 'DISABLED'));
CREATE UNIQUE INDEX organization_units_sibling_name_idx
    ON organization_units (organization_id, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), name);
ALTER TABLE organization_units DROP COLUMN deleted_at;
ALTER TABLE organizations DROP COLUMN structure_version;
