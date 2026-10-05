DROP TRIGGER role_bindings_initial_start ON role_bindings;
DROP TRIGGER role_assignments_initial_start ON role_assignments;
DROP FUNCTION initialize_role_binding_start();

CREATE OR REPLACE FUNCTION mirror_legacy_role_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM role_assignments WHERE id = OLD.id AND source = 'LEGACY_ROLE_BINDING';
        RETURN OLD;
    END IF;
    INSERT INTO role_assignments(id, user_id, role_code, organization_id, unit_id, created_by, source, created_at)
    SELECT NEW.id, m.user_id, NEW.role, m.organization_id, NEW.unit_id, NEW.created_by, 'LEGACY_ROLE_BINDING', NEW.created_at
    FROM memberships m WHERE m.id = NEW.membership_id
    ON CONFLICT (id) DO UPDATE SET
        user_id = EXCLUDED.user_id,
        role_code = EXCLUDED.role_code,
        organization_id = EXCLUDED.organization_id,
        unit_id = EXCLUDED.unit_id,
        created_by = EXCLUDED.created_by;
    RETURN NEW;
END;
$$;

DELETE FROM role_bindings WHERE revoked_at IS NOT NULL;
DELETE FROM role_assignments WHERE revoked_at IS NOT NULL;

DROP INDEX role_assignments_idempotency_idx;
DROP INDEX role_assignments_effective_idx;
DROP INDEX role_assignments_active_unique_idx;
ALTER TABLE role_assignments
    DROP CONSTRAINT role_assignments_revocation_check,
    DROP CONSTRAINT role_assignments_validity_check,
    DROP CONSTRAINT role_assignments_grant_source_check,
    DROP CONSTRAINT role_assignments_idempotency_key_check,
    DROP COLUMN idempotency_key,
    DROP COLUMN grant_source,
    DROP COLUMN revoked_by,
    DROP COLUMN revoked_at,
    DROP COLUMN expires_at,
    DROP COLUMN starts_at;
CREATE UNIQUE INDEX role_assignments_unique_idx
    ON role_assignments (
        user_id,
        role_code,
        COALESCE(organization_id, '00000000-0000-0000-0000-000000000000'::uuid),
        COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'::uuid)
    );

DROP INDEX role_bindings_expiry_idx;
DROP INDEX role_bindings_idempotency_idx;
DROP INDEX role_bindings_active_unique_idx;
ALTER TABLE role_bindings
    DROP CONSTRAINT role_bindings_idempotency_key_check,
    DROP CONSTRAINT role_bindings_revocation_check,
    DROP CONSTRAINT role_bindings_validity_check,
    DROP CONSTRAINT role_bindings_grant_source_check,
    DROP COLUMN idempotency_key,
    DROP COLUMN grant_source,
    DROP COLUMN revoked_by,
    DROP COLUMN revoked_at,
    DROP COLUMN expires_at,
    DROP COLUMN starts_at;
CREATE UNIQUE INDEX role_bindings_unique_idx
    ON role_bindings (membership_id, role, COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'::uuid));
