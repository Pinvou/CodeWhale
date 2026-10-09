ALTER TABLE payment_orders
    ADD COLUMN beneficiary_organization_id UUID;

ALTER TABLE entitlement_components
    ADD CONSTRAINT entitlement_components_grant_identity_unique UNIQUE (id,package_id,component_type);

CREATE TABLE member_quota_grants (
    id UUID PRIMARY KEY,
    organization_id UUID NOT NULL,
    member_user_id UUID NOT NULL,
    package_id UUID NOT NULL REFERENCES entitlement_packages(id),
    component_id UUID NOT NULL REFERENCES entitlement_components(id),
    component_type TEXT NOT NULL CHECK (component_type IN ('BASE','BONUS')),
    original_microcredits BIGINT NOT NULL CHECK (original_microcredits > 0),
    available_microcredits BIGINT NOT NULL CHECK (available_microcredits >= 0),
    reserved_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (reserved_microcredits >= 0),
    used_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (used_microcredits >= 0),
    expired_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (expired_microcredits >= 0),
    revoked_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (revoked_microcredits >= 0),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE','SUSPENDED','REVOKED','EXPIRED','RECLAIMED_ACCOUNT_CLOSED')),
    suspension_reason TEXT CHECK (suspension_reason IN ('MEMBER_REMOVED','ACCOUNT_CLOSED')),
    membership_version BIGINT NOT NULL CHECK (membership_version > 0),
    allocation_version BIGINT NOT NULL DEFAULT 1 CHECK (allocation_version > 0),
    created_by UUID NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
    request_fingerprint TEXT NOT NULL CHECK (char_length(request_fingerprint)=64),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (component_id,package_id,component_type)
        REFERENCES entitlement_components(id,package_id,component_type),
    UNIQUE (organization_id,idempotency_key),
    CHECK (available_microcredits + reserved_microcredits + used_microcredits + expired_microcredits + revoked_microcredits = original_microcredits),
    CHECK ((status='SUSPENDED' AND suspension_reason IS NOT NULL) OR (status<>'SUSPENDED' AND suspension_reason IS NULL))
);

CREATE INDEX member_quota_grants_member_idx
    ON member_quota_grants(organization_id,member_user_id,status,expires_at,id);
CREATE INDEX member_quota_grants_component_idx
    ON member_quota_grants(component_id,status,expires_at,id);

CREATE TABLE member_quota_revoke_commands (
    organization_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
    request_fingerprint TEXT NOT NULL CHECK (char_length(request_fingerprint)=64),
    grant_id UUID NOT NULL REFERENCES member_quota_grants(id),
    response_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id,idempotency_key)
);

CREATE TABLE member_quota_hold_allocations (
    hold_id UUID NOT NULL REFERENCES usage_holds(id),
    grant_id UUID NOT NULL REFERENCES member_quota_grants(id),
    component_id UUID NOT NULL REFERENCES entitlement_components(id),
    reserved_microcredits BIGINT NOT NULL CHECK (reserved_microcredits > 0),
    settled_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (settled_microcredits >= 0),
    released_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (released_microcredits >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (hold_id,grant_id),
    CHECK (settled_microcredits + released_microcredits <= reserved_microcredits)
);

CREATE INDEX member_quota_hold_allocations_grant_idx
    ON member_quota_hold_allocations(grant_id,hold_id);

CREATE TABLE member_quota_grant_audits (
    id UUID PRIMARY KEY,
    grant_id UUID NOT NULL REFERENCES member_quota_grants(id),
    organization_id UUID NOT NULL,
    member_user_id UUID NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('ALLOCATED','SUSPENDED','RESTORED','REVOKED','HOLD_RESERVED','HOLD_RELEASED','HOLD_SETTLED','EXPIRED')),
    actor_user_id UUID,
    amount_microcredits BIGINT NOT NULL CHECK (amount_microcredits >= 0),
    snapshot JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX member_quota_grant_audits_grant_idx
    ON member_quota_grant_audits(grant_id,occurred_at,id);
