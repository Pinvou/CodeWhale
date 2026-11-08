CREATE TABLE organization_member_lifecycle_states (
    organization_id UUID NOT NULL,
    user_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE','REVOKED')),
    membership_version BIGINT NOT NULL CHECK (membership_version > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id,user_id)
);
