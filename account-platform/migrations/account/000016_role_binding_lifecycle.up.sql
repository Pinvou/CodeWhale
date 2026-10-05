ALTER TABLE role_bindings
    ADD COLUMN starts_at TIMESTAMPTZ,
    ADD COLUMN expires_at TIMESTAMPTZ,
    ADD COLUMN revoked_at TIMESTAMPTZ,
    ADD COLUMN revoked_by UUID REFERENCES users(id),
    ADD COLUMN grant_source TEXT NOT NULL DEFAULT 'ACCOUNT',
    ADD COLUMN idempotency_key TEXT;

UPDATE role_bindings SET starts_at = created_at WHERE starts_at IS NULL;

ALTER TABLE role_bindings
    ALTER COLUMN starts_at SET NOT NULL,
    ADD CONSTRAINT role_bindings_validity_check CHECK (expires_at IS NULL OR expires_at > starts_at),
    ADD CONSTRAINT role_bindings_revocation_check CHECK (revoked_at IS NULL OR revoked_at >= created_at),
    ADD CONSTRAINT role_bindings_grant_source_check CHECK (char_length(grant_source) BETWEEN 1 AND 80),
    ADD CONSTRAINT role_bindings_idempotency_key_check CHECK (idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 8 AND 120);

DROP INDEX role_bindings_unique_idx;
CREATE UNIQUE INDEX role_bindings_active_unique_idx
    ON role_bindings (membership_id, role, COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX role_bindings_idempotency_idx
    ON role_bindings (membership_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX role_bindings_expiry_idx
    ON role_bindings (expires_at, id)
    WHERE revoked_at IS NULL AND expires_at IS NOT NULL;

ALTER TABLE role_assignments
    ADD COLUMN starts_at TIMESTAMPTZ,
    ADD COLUMN expires_at TIMESTAMPTZ,
    ADD COLUMN revoked_at TIMESTAMPTZ,
    ADD COLUMN revoked_by UUID REFERENCES users(id),
    ADD COLUMN grant_source TEXT NOT NULL DEFAULT 'ACCOUNT',
    ADD COLUMN idempotency_key TEXT;

UPDATE role_assignments SET starts_at = created_at WHERE starts_at IS NULL;

ALTER TABLE role_assignments
    ALTER COLUMN starts_at SET NOT NULL,
    ADD CONSTRAINT role_assignments_validity_check CHECK (expires_at IS NULL OR expires_at > starts_at),
    ADD CONSTRAINT role_assignments_revocation_check CHECK (revoked_at IS NULL OR revoked_at >= created_at),
    ADD CONSTRAINT role_assignments_grant_source_check CHECK (char_length(grant_source) BETWEEN 1 AND 80),
    ADD CONSTRAINT role_assignments_idempotency_key_check CHECK (idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 8 AND 120);

DROP INDEX role_assignments_unique_idx;
CREATE UNIQUE INDEX role_assignments_active_unique_idx
    ON role_assignments (
        user_id,
        role_code,
        COALESCE(organization_id, '00000000-0000-0000-0000-000000000000'::uuid),
        COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'::uuid)
    ) WHERE revoked_at IS NULL;
CREATE INDEX role_assignments_effective_idx
    ON role_assignments (user_id, organization_id, starts_at, expires_at)
    WHERE revoked_at IS NULL;

-- Existing member and ownership writers already supply their authoritative
-- creation time. Preserve that start time when they omit the new field.
CREATE FUNCTION initialize_role_binding_start() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.starts_at := COALESCE(NEW.starts_at, NEW.created_at);
    RETURN NEW;
END;
$$;
CREATE TRIGGER role_bindings_initial_start
BEFORE INSERT ON role_bindings
FOR EACH ROW EXECUTE FUNCTION initialize_role_binding_start();
CREATE TRIGGER role_assignments_initial_start
BEFORE INSERT ON role_assignments
FOR EACH ROW EXECUTE FUNCTION initialize_role_binding_start();
CREATE UNIQUE INDEX role_assignments_idempotency_idx
    ON role_assignments (user_id, COALESCE(organization_id, '00000000-0000-0000-0000-000000000000'::uuid), idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE OR REPLACE FUNCTION mirror_legacy_role_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM role_assignments WHERE id = OLD.id AND source = 'LEGACY_ROLE_BINDING';
        RETURN OLD;
    END IF;
    INSERT INTO role_assignments(
        id, user_id, role_code, organization_id, unit_id, created_by, source, grant_source,
        starts_at, expires_at, revoked_at, revoked_by, idempotency_key, created_at
    )
    SELECT NEW.id, m.user_id, NEW.role, m.organization_id, NEW.unit_id, NEW.created_by,
           'LEGACY_ROLE_BINDING', NEW.grant_source, NEW.starts_at, NEW.expires_at,
           NEW.revoked_at, NEW.revoked_by, NEW.idempotency_key, NEW.created_at
    FROM memberships m WHERE m.id = NEW.membership_id
    ON CONFLICT (id) DO UPDATE SET
        user_id = EXCLUDED.user_id,
        role_code = EXCLUDED.role_code,
        organization_id = EXCLUDED.organization_id,
        unit_id = EXCLUDED.unit_id,
        created_by = EXCLUDED.created_by,
        grant_source = EXCLUDED.grant_source,
        starts_at = EXCLUDED.starts_at,
        expires_at = EXCLUDED.expires_at,
        revoked_at = EXCLUDED.revoked_at,
        revoked_by = EXCLUDED.revoked_by,
        idempotency_key = EXCLUDED.idempotency_key;
    RETURN NEW;
END;
$$;
