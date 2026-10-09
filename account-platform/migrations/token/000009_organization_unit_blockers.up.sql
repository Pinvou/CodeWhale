CREATE TABLE organization_unit_governance_blockers (
    organization_id UUID NOT NULL,
    unit_id UUID NOT NULL,
    blocker_code TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    details JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id, unit_id, blocker_code, resource_type, resource_id)
);

CREATE INDEX organization_unit_governance_blockers_active_idx
    ON organization_unit_governance_blockers (organization_id, unit_id, blocker_code)
    WHERE active;

CREATE TABLE organization_unit_lifecycle_states (
    organization_id UUID NOT NULL,
    unit_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'DELETION_PENDING', 'DELETED')),
    structure_version BIGINT NOT NULL CHECK (structure_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id, unit_id)
);
