ALTER TABLE organizations
    ADD COLUMN deleted_at TIMESTAMPTZ;

CREATE UNIQUE INDEX organizations_active_name_idx
    ON organizations (lower(name))
    WHERE status <> 'DELETED';

CREATE TABLE organization_creation_requests (
    actor_user_id UUID NOT NULL REFERENCES users(id),
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 120),
    request_hash TEXT NOT NULL CHECK (char_length(request_hash) = 64),
    organization_id UUID NOT NULL UNIQUE REFERENCES organizations(id),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (actor_user_id, idempotency_key)
);

CREATE FUNCTION protect_organization_creation_request() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'organization creation idempotency records are immutable';
END;
$$;

CREATE TRIGGER organization_creation_requests_immutable
BEFORE UPDATE OR DELETE ON organization_creation_requests
FOR EACH ROW EXECUTE FUNCTION protect_organization_creation_request();

CREATE FUNCTION protect_organization_lifecycle() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'organizations must be logically deleted';
    END IF;
    IF NEW.id <> OLD.id OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'organization identity is immutable';
    END IF;
    IF OLD.status = 'DELETED' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'deleted organization is immutable';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
        (OLD.status='ACTIVE' AND NEW.status='DISABLED') OR
        (OLD.status='DISABLED' AND NEW.status IN ('ACTIVE','DELETED'))
    ) THEN
        RAISE EXCEPTION 'invalid organization lifecycle transition';
    END IF;
    IF (NEW.name <> OLD.name OR NEW.status <> OLD.status) AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'organization mutations must increment version exactly once';
    END IF;
    IF (NEW.status='DELETED') <> (NEW.deleted_at IS NOT NULL) THEN
        RAISE EXCEPTION 'deleted organization timestamp must match status';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER organizations_lifecycle_guard
BEFORE UPDATE OR DELETE ON organizations
FOR EACH ROW EXECUTE FUNCTION protect_organization_lifecycle();
