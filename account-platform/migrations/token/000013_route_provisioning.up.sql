CREATE TABLE route_provisionings (
    id UUID PRIMARY KEY,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('PERSONAL','ORGANIZATION')),
    scope_id UUID NOT NULL,
    wallet_id UUID NOT NULL,
    model_id UUID NOT NULL REFERENCES model_identities(id),
    upstream_model_ref TEXT NOT NULL,
    protocol_code TEXT NOT NULL,
    desired_generation BIGINT NOT NULL CHECK (desired_generation > 0),
    actual_generation BIGINT CHECK (actual_generation IS NULL OR actual_generation > 0),
    desired_signature TEXT NOT NULL CHECK (desired_signature ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK (status IN ('PENDING','READY','FAILED','REPAIR_REQUIRED')),
    stage TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    policy_snapshot JSONB NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    repair_round INTEGER NOT NULL DEFAULT 0 CHECK (repair_round >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts > 0),
    sla_deadline_at TIMESTAMPTZ NOT NULL,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    next_retry_at TIMESTAMPTZ,
    last_error_code TEXT,
    last_error_detail TEXT,
    active_token_account_id UUID REFERENCES token_accounts(id),
    active_credential_id UUID REFERENCES provider_credentials(id),
    requested_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(scope_type,scope_id,model_id,protocol_code,desired_generation)
);

CREATE INDEX route_provisionings_signature_idx
    ON route_provisionings(scope_type,scope_id,model_id,protocol_code,desired_signature);
CREATE INDEX route_provisionings_due_idx
    ON route_provisionings(status,next_retry_at,lease_until)
    WHERE status IN ('PENDING','FAILED');

CREATE TABLE route_provisioning_attempts (
    id UUID PRIMARY KEY,
    provisioning_id UUID NOT NULL REFERENCES route_provisionings(id),
    repair_round INTEGER NOT NULL CHECK (repair_round >= 0),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    worker_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('RUNNING','SUCCEEDED','FAILED','TIMED_OUT')),
    stage TEXT NOT NULL,
    error_code TEXT,
    error_detail TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    UNIQUE(provisioning_id,repair_round,attempt_number)
);

CREATE TABLE route_provisioning_commands (
    idempotency_key TEXT PRIMARY KEY,
    command_type TEXT NOT NULL,
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    provisioning_id UUID NOT NULL REFERENCES route_provisionings(id),
    repair_round INTEGER NOT NULL CHECK (repair_round > 0),
    actor_id UUID NOT NULL,
    reason TEXT NOT NULL,
    result_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE route_provisioning_audit (
    id UUID PRIMARY KEY,
    provisioning_id UUID NOT NULL REFERENCES route_provisionings(id),
    action TEXT NOT NULL,
    before_status TEXT,
    after_status TEXT,
    desired_generation BIGINT NOT NULL,
    actor_id UUID NOT NULL,
    reason TEXT NOT NULL,
    evidence JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE organization_model_policies (
    organization_id UUID PRIMARY KEY,
    version BIGINT NOT NULL CHECK (version > 0),
    mode TEXT NOT NULL CHECK (mode IN ('ALLOW_ALL','ALLOW_LIST','DENY_LIST')),
    model_ids UUID[] NOT NULL DEFAULT '{}',
    authorization_version BIGINT NOT NULL CHECK (authorization_version > 0),
    updated_by UUID NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE organization_model_policy_commands (
    organization_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    result_version BIGINT NOT NULL CHECK (result_version > 0),
    actor_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(organization_id,idempotency_key)
);

CREATE TABLE organization_model_policy_audit (
    id UUID PRIMARY KEY,
    organization_id UUID NOT NULL,
    policy_version BIGINT NOT NULL,
    actor_id UUID NOT NULL,
    authorization_version BIGINT NOT NULL,
    reason TEXT NOT NULL,
    snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TRIGGER route_provisioning_audit_immutable BEFORE UPDATE OR DELETE ON route_provisioning_audit FOR EACH ROW EXECUTE FUNCTION reject_model_catalog_history_mutation();
CREATE TRIGGER organization_model_policy_audit_immutable BEFORE UPDATE OR DELETE ON organization_model_policy_audit FOR EACH ROW EXECUTE FUNCTION reject_model_catalog_history_mutation();
