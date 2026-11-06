CREATE TABLE organization_lifecycle_states (
    organization_id UUID PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'DISABLED', 'DELETED')),
    account_version BIGINT NOT NULL CHECK (account_version > 0),
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE FUNCTION prevent_deleted_organization_reactivation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'DELETED' AND NEW.status <> 'DELETED' THEN
        RAISE EXCEPTION 'deleted organization lifecycle is terminal';
    END IF;
    IF NEW.account_version < OLD.account_version THEN
        RAISE EXCEPTION 'organization lifecycle version cannot decrease';
    END IF;
    IF NEW.status <> OLD.status AND NEW.account_version <= OLD.account_version THEN
        RAISE EXCEPTION 'organization lifecycle change requires a newer account version';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER organization_lifecycle_terminal_guard
BEFORE UPDATE ON organization_lifecycle_states
FOR EACH ROW EXECUTE FUNCTION prevent_deleted_organization_reactivation();
