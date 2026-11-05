ALTER TABLE organizations
    ADD COLUMN structure_version BIGINT NOT NULL DEFAULT 1 CHECK (structure_version > 0);

ALTER TABLE organization_units
    ADD COLUMN deleted_at TIMESTAMPTZ;

DROP INDEX organization_units_sibling_name_idx;
CREATE UNIQUE INDEX organization_units_active_sibling_name_idx
    ON organization_units (
        organization_id,
        COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid),
        lower(name)
    )
    WHERE status <> 'DELETED';

ALTER TABLE organization_units DROP CONSTRAINT organization_units_status_check;
ALTER TABLE organization_units
    ADD CONSTRAINT organization_units_status_check CHECK (status IN ('ACTIVE', 'DISABLED', 'DELETED'));

CREATE TABLE organization_unit_commands (
    actor_user_id UUID NOT NULL REFERENCES users(id),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    command_type TEXT NOT NULL CHECK (command_type IN ('CREATE', 'RENAME', 'MOVE', 'DELETE')),
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 120),
    request_hash TEXT NOT NULL CHECK (char_length(request_hash) = 64),
    unit_id UUID NOT NULL,
    result JSONB NOT NULL CHECK (jsonb_typeof(result) = 'object'),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (actor_user_id, organization_id, command_type, idempotency_key),
    FOREIGN KEY (organization_id, unit_id) REFERENCES organization_units(organization_id, id)
);

CREATE TABLE organization_unit_governance_blockers (
    organization_id UUID NOT NULL REFERENCES organizations(id),
    unit_id UUID NOT NULL,
    blocker_code TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    details JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id, unit_id, blocker_code, resource_type, resource_id),
    FOREIGN KEY (organization_id, unit_id) REFERENCES organization_units(organization_id, id)
);

CREATE INDEX organization_unit_governance_blockers_active_idx
    ON organization_unit_governance_blockers (organization_id, unit_id, blocker_code)
    WHERE active;

CREATE FUNCTION protect_organization_unit_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'organization units are retained permanently';
    END IF;
    IF OLD.id IS DISTINCT FROM NEW.id OR OLD.organization_id IS DISTINCT FROM NEW.organization_id THEN
        RAISE EXCEPTION 'organization unit identity is immutable';
    END IF;
    IF OLD.status = 'DELETED' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'deleted organization units are immutable';
    END IF;
    IF NEW.status = 'DELETED' AND OLD.status <> 'DELETED' AND NEW.deleted_at IS NULL THEN
        RAISE EXCEPTION 'deleted organization units require deleted_at';
    END IF;
    IF NEW.status <> 'DELETED' AND NEW.deleted_at IS NOT NULL THEN
        RAISE EXCEPTION 'only deleted organization units may have deleted_at';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER organization_units_lifecycle_guard
BEFORE UPDATE OR DELETE ON organization_units
FOR EACH ROW EXECUTE FUNCTION protect_organization_unit_lifecycle();

CREATE FUNCTION protect_organization_unit_command() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'organization unit command records are immutable';
END;
$$;

CREATE TRIGGER organization_unit_commands_immutable
BEFORE UPDATE OR DELETE ON organization_unit_commands
FOR EACH ROW EXECUTE FUNCTION protect_organization_unit_command();
