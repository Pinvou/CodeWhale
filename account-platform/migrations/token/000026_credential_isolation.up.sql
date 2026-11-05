ALTER TABLE provider_credentials DROP CONSTRAINT provider_credentials_status_check;
ALTER TABLE provider_credentials ADD CONSTRAINT provider_credentials_status_check
    CHECK (status IN ('PENDING','CANDIDATE','ACTIVE','RETIRING','RETIRED','REVOKED'));

CREATE TABLE credential_launches (
    id UUID PRIMARY KEY,
    result_ref UUID NOT NULL UNIQUE,
    actor_user_id UUID NOT NULL,
    actor_issuer TEXT NOT NULL CHECK (length(actor_issuer) BETWEEN 1 AND 512),
    actor_subject TEXT NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 512),
    authorization_version BIGINT NOT NULL CHECK (authorization_version > 0),
    gate_epoch BIGINT NOT NULL CHECK (gate_epoch > 0),
    token_account_id UUID NOT NULL REFERENCES token_accounts(id),
    action TEXT NOT NULL CHECK (action IN ('ENROLL','ROTATE')),
    expected_account_version BIGINT NOT NULL CHECK (expected_account_version > 0),
    expected_credential_version INTEGER NOT NULL CHECK (expected_credential_version >= 0),
    slot TEXT NOT NULL CHECK (slot ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    idempotency_key_digest TEXT NOT NULL CHECK (idempotency_key_digest ~ '^[0-9a-f]{64}$'),
    browser_binding_digest TEXT CHECK (browser_binding_digest ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK (status IN ('CREATED','CONSUMED','AUTHENTICATED','COMPLETED','FAILED','REVOKED','EXPIRED')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 3),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    heartbeat_deadline TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    stable_error_code TEXT,
    UNIQUE(actor_user_id,action,idempotency_key_digest),
    CHECK (expires_at = created_at + interval '5 minutes'),
    CHECK (stable_error_code IS NULL OR stable_error_code ~ '^[A-Z][A-Z0-9_]{0,63}$')
);

CREATE INDEX credential_launches_expiry_idx
    ON credential_launches(expires_at,id)
    WHERE status IN ('CREATED','CONSUMED','AUTHENTICATED');

CREATE TABLE credential_isolation_sessions (
    id UUID PRIMARY KEY,
    launch_id UUID NOT NULL UNIQUE REFERENCES credential_launches(id),
    actor_user_id UUID NOT NULL,
    authorization_version BIGINT NOT NULL CHECK (authorization_version > 0),
    gate_epoch BIGINT NOT NULL CHECK (gate_epoch > 0),
    credential_origin TEXT NOT NULL CHECK (credential_origin ~ '^https?://[^/?#]+$'),
    browser_binding_digest TEXT NOT NULL CHECK (browser_binding_digest ~ '^[0-9a-f]{64}$'),
    state_digest TEXT NOT NULL CHECK (state_digest ~ '^[0-9a-f]{64}$'),
    nonce_digest TEXT NOT NULL CHECK (nonce_digest ~ '^[0-9a-f]{64}$'),
    pkce_verifier_ciphertext BYTEA NOT NULL,
    key_id TEXT NOT NULL,
    csrf_digest TEXT NOT NULL CHECK (csrf_digest ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK (status IN ('AUTHENTICATING','READY','COMPLETED','FAILED','REVOKED','EXPIRED')),
    auth_time TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at <= created_at + interval '5 minutes')
);

CREATE INDEX credential_isolation_sessions_expiry_idx
    ON credential_isolation_sessions(expires_at,id)
    WHERE status IN ('AUTHENTICATING','READY');

CREATE TABLE credential_isolation_results (
    result_ref UUID PRIMARY KEY,
    launch_id UUID NOT NULL UNIQUE REFERENCES credential_launches(id),
    actor_user_id UUID NOT NULL,
    token_account_id UUID NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('ENROLL','ROTATE')),
    status TEXT NOT NULL CHECK (status IN ('PENDING','SUCCEEDED','FAILED','REVOKED','EXPIRED')),
    credential_id UUID,
    credential_version INTEGER,
    credential_status TEXT,
    validation_status TEXT,
    fingerprint TEXT,
    stable_error_code TEXT,
    audit_request_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK ((status='SUCCEEDED') = (credential_id IS NOT NULL)),
    CHECK (stable_error_code IS NULL OR stable_error_code ~ '^[A-Z][A-Z0-9_]{0,63}$')
);

CREATE OR REPLACE FUNCTION reject_credential_result_identity_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.result_ref<>OLD.result_ref OR NEW.launch_id<>OLD.launch_id OR NEW.actor_user_id<>OLD.actor_user_id
       OR NEW.token_account_id<>OLD.token_account_id OR NEW.action<>OLD.action OR NEW.created_at<>OLD.created_at
       OR NEW.audit_request_id<>OLD.audit_request_id THEN
        RAISE EXCEPTION 'credential isolation result identity is immutable';
    END IF;
    IF OLD.status<>'PENDING' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'terminal credential isolation result is immutable';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER credential_isolation_result_immutable
    BEFORE UPDATE ON credential_isolation_results
    FOR EACH ROW EXECUTE FUNCTION reject_credential_result_identity_mutation();

-- A validation probe is claimed durably before the provider call. The lease is
-- an observation/recovery deadline, not permission to issue a second probe:
-- once claimed, the same command reference can only recover its persisted
-- terminal result or remain UNKNOWN.
ALTER TABLE token_command_results
    ADD COLUMN credential_probe_claimed_at TIMESTAMPTZ,
    ADD COLUMN credential_probe_lease_until TIMESTAMPTZ,
    ADD CONSTRAINT token_command_credential_probe_claim_check CHECK (
        (credential_probe_claimed_at IS NULL AND credential_probe_lease_until IS NULL)
        OR (command_type='TOKEN_CREDENTIAL_VALIDATE'
            AND credential_probe_claimed_at IS NOT NULL
            AND credential_probe_lease_until=credential_probe_claimed_at + interval '30 seconds')
    );
