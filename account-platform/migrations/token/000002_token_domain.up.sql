CREATE TABLE token_accounts (
    id UUID PRIMARY KEY,
    owner_type TEXT NOT NULL CHECK (owner_type IN ('USER', 'ORG', 'ORG_UNIT')),
    owner_id UUID NOT NULL,
    owner_organization_id UUID,
    owner_unit_id UUID,
    wallet_id UUID NOT NULL,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'DISABLED', 'FROZEN')),
    default_priority INTEGER NOT NULL DEFAULT 100 CHECK (default_priority BETWEEN 0 AND 10000),
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (owner_type, owner_id, name),
    CHECK ((owner_type = 'USER') = (owner_organization_id IS NULL)),
    CHECK ((owner_type = 'ORG_UNIT') = (owner_unit_id IS NOT NULL))
);

CREATE TABLE provider_credentials (
    id UUID PRIMARY KEY,
    token_account_id UUID NOT NULL REFERENCES token_accounts(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    base_url TEXT NOT NULL,
    ciphertext BYTEA NOT NULL,
    key_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'ACTIVE', 'RETIRED', 'REVOKED')),
    allowed_models TEXT[] NOT NULL,
    validated_at TIMESTAMPTZ,
    activated_at TIMESTAMPTZ,
    retired_at TIMESTAMPTZ,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (token_account_id, version),
    UNIQUE (token_account_id, fingerprint),
    CHECK (cardinality(allowed_models) > 0)
);

CREATE UNIQUE INDEX provider_credentials_active_idx
    ON provider_credentials (token_account_id)
    WHERE status = 'ACTIVE';

CREATE TABLE token_share_policies (
    token_account_id UUID PRIMARY KEY REFERENCES token_accounts(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('OWNER_UNIT_ONLY', 'OWNER_SUBTREE', 'ORG_ALL', 'EXPLICIT_MEMBERS')),
    owner_organization_id UUID,
    owner_unit_id UUID,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE token_explicit_members (
    token_account_id UUID NOT NULL REFERENCES token_accounts(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (token_account_id, user_id)
);

CREATE TABLE routing_policies (
    id UUID PRIMARY KEY,
    token_account_id UUID NOT NULL REFERENCES token_accounts(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    priority INTEGER NOT NULL CHECK (priority BETWEEN 0 AND 10000),
    default_scope TEXT NOT NULL CHECK (default_scope IN ('NONE', 'UNIT', 'ORG')),
    health_status TEXT NOT NULL CHECK (health_status IN ('HEALTHY', 'DEGRADED', 'UNHEALTHY')),
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (token_account_id, model)
);

CREATE TABLE usage_requests (
    id UUID PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    user_id UUID NOT NULL,
    session_id TEXT NOT NULL,
    organization_id UUID,
    token_account_id UUID NOT NULL REFERENCES token_accounts(id),
    credential_id UUID NOT NULL REFERENCES provider_credentials(id),
    wallet_id UUID NOT NULL,
    price_version_id UUID,
    hold_id UUID,
    model TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('CREATED', 'RESERVED', 'UPSTREAM_ACTIVE', 'SETTLEMENT_PENDING', 'COMMITTED', 'RELEASED', 'REVIEW')),
    maximum_microcredits BIGINT NOT NULL CHECK (maximum_microcredits > 0),
    actual_microcredits BIGINT CHECK (actual_microcredits >= 0),
    request_snapshot JSONB NOT NULL,
    usage_snapshot JSONB,
    error_class TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    terminal_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX usage_requests_pending_idx
    ON usage_requests (updated_at)
    WHERE status IN ('RESERVED', 'UPSTREAM_ACTIVE', 'SETTLEMENT_PENDING', 'REVIEW');

CREATE TABLE usage_attempts (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL REFERENCES usage_requests(id) ON DELETE CASCADE,
    newapi_request_id TEXT,
    provider_request_id TEXT,
    status TEXT NOT NULL CHECK (status IN ('STARTED', 'STREAMING', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
    input_tokens BIGINT CHECK (input_tokens >= 0),
    output_tokens BIGINT CHECK (output_tokens >= 0),
    cached_input_tokens BIGINT CHECK (cached_input_tokens >= 0),
    error_class TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ
);

CREATE TABLE authorization_cache_versions (
    organization_id UUID PRIMARY KEY,
    membership_version BIGINT NOT NULL CHECK (membership_version > 0),
    synchronized_at TIMESTAMPTZ NOT NULL
);
