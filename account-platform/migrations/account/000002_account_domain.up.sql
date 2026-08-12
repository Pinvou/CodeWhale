CREATE TABLE users (
    id UUID PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('PROVISIONING', 'ACTIVE', 'DISABLED')),
    display_name TEXT,
    email TEXT,
    membership_version BIGINT NOT NULL DEFAULT 1 CHECK (membership_version > 0),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE identities (
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    user_id UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (issuer, subject)
);

CREATE TABLE organizations (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'DISABLED')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE organization_units (
    id UUID PRIMARY KEY,
    organization_id UUID NOT NULL REFERENCES organizations(id),
    parent_id UUID,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'DISABLED')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (organization_id, id),
    FOREIGN KEY (organization_id, parent_id) REFERENCES organization_units(organization_id, id)
);

CREATE UNIQUE INDEX organization_units_sibling_name_idx
    ON organization_units (organization_id, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), name);

CREATE TABLE organization_unit_closure (
    organization_id UUID NOT NULL,
    ancestor_id UUID NOT NULL,
    descendant_id UUID NOT NULL,
    depth INTEGER NOT NULL CHECK (depth >= 0),
    PRIMARY KEY (organization_id, ancestor_id, descendant_id),
    FOREIGN KEY (organization_id, ancestor_id) REFERENCES organization_units(organization_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, descendant_id) REFERENCES organization_units(organization_id, id) ON DELETE CASCADE,
    CHECK ((depth = 0) = (ancestor_id = descendant_id))
);

CREATE INDEX organization_unit_closure_descendant_idx
    ON organization_unit_closure (organization_id, descendant_id, depth);

CREATE TABLE memberships (
    id UUID PRIMARY KEY,
    organization_id UUID NOT NULL REFERENCES organizations(id),
    user_id UUID NOT NULL REFERENCES users(id),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'SUSPENDED', 'REVOKED')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    joined_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (organization_id, user_id)
);

CREATE TABLE membership_units (
    membership_id UUID NOT NULL REFERENCES memberships(id) ON DELETE CASCADE,
    unit_id UUID NOT NULL REFERENCES organization_units(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (membership_id, unit_id)
);

CREATE TABLE role_bindings (
    id UUID PRIMARY KEY,
    membership_id UUID NOT NULL REFERENCES memberships(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('OWNER', 'ORG_ADMIN', 'BILLING_ADMIN', 'UNIT_ADMIN', 'MEMBER', 'AUDITOR')),
    unit_id UUID REFERENCES organization_units(id),
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL,
    CHECK ((role = 'UNIT_ADMIN') = (unit_id IS NOT NULL))
);

CREATE UNIQUE INDEX role_bindings_unique_idx
    ON role_bindings (membership_id, role, COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'::uuid));

CREATE TABLE invitations (
    id UUID PRIMARY KEY,
    organization_id UUID NOT NULL REFERENCES organizations(id),
    email_normalized TEXT NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'ACCEPTED', 'REVOKED', 'EXPIRED')),
    roles JSONB NOT NULL,
    invited_by UUID NOT NULL REFERENCES users(id),
    accepted_by UUID REFERENCES users(id),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX invitations_pending_email_idx
    ON invitations (email_normalized, expires_at)
    WHERE status = 'PENDING';

CREATE TABLE user_sessions (
    issuer TEXT NOT NULL,
    session_id TEXT NOT NULL,
    user_id UUID NOT NULL REFERENCES users(id),
    active_context_type TEXT NOT NULL CHECK (active_context_type IN ('PERSONAL', 'ORGANIZATION')),
    active_organization_id UUID REFERENCES organizations(id),
    membership_version BIGINT NOT NULL CHECK (membership_version > 0),
    last_seen_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (issuer, session_id),
    CHECK ((active_context_type = 'ORGANIZATION') = (active_organization_id IS NOT NULL))
);

CREATE INDEX user_sessions_user_idx ON user_sessions (user_id, last_seen_at DESC);

ALTER TABLE audit_logs
    ADD CONSTRAINT audit_logs_actor_user_fk FOREIGN KEY (actor_user_id) REFERENCES users(id);
