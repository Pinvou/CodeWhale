ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('PROVISIONING', 'ACTIVE', 'REPAIR_REQUIRED', 'DISABLED', 'DELETED'));

CREATE TABLE registration_operations (
    id UUID PRIMARY KEY,
    candidate_user_id UUID NOT NULL UNIQUE,
    query_token_hash BYTEA NOT NULL CHECK (octet_length(query_token_hash) = 32),
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    username_normalized TEXT NOT NULL,
    mobile_e164 TEXT NOT NULL,
    sms_challenge_id UUID NOT NULL UNIQUE,
    password_policy_version TEXT NOT NULL,
    identity_subject TEXT,
    committed_user_id UUID REFERENCES users(id),
    status TEXT NOT NULL CHECK (status IN (
        'SMS_PENDING', 'REGISTRATION_IN_PROGRESS', 'IDENTITY_CREATING',
        'IDENTITY_CREATED', 'ACCOUNT_COMMITTING', 'ACCOUNT_COMMITTED',
        'PROVISIONING', 'ACTIVE', 'REPAIR_REQUIRED', 'COMPENSATION_PENDING',
        'COMPENSATED', 'EXPIRED'
    )),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    lease_expires_at TIMESTAMPTZ,
    identity_created_at TIMESTAMPTZ,
    account_committed_at TIMESTAMPTZ,
    next_retry_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error_code TEXT,
    compensation_evidence JSONB,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK ((status = 'IDENTITY_CREATED') IS NOT TRUE OR identity_subject IS NOT NULL),
    CHECK ((status IN ('ACCOUNT_COMMITTED', 'PROVISIONING', 'ACTIVE')) IS NOT TRUE OR committed_user_id IS NOT NULL)
);

CREATE UNIQUE INDEX registration_operations_active_username_idx
    ON registration_operations (username_normalized)
    WHERE status IN ('SMS_PENDING', 'REGISTRATION_IN_PROGRESS', 'IDENTITY_CREATING', 'IDENTITY_CREATED', 'ACCOUNT_COMMITTING', 'REPAIR_REQUIRED', 'COMPENSATION_PENDING');

CREATE UNIQUE INDEX registration_operations_active_mobile_idx
    ON registration_operations (mobile_e164)
    WHERE status IN ('SMS_PENDING', 'REGISTRATION_IN_PROGRESS', 'IDENTITY_CREATING', 'IDENTITY_CREATED', 'ACCOUNT_COMMITTING', 'REPAIR_REQUIRED', 'COMPENSATION_PENDING');

CREATE INDEX registration_operations_recovery_idx
    ON registration_operations (next_retry_at, updated_at)
    WHERE status IN ('REGISTRATION_IN_PROGRESS', 'IDENTITY_CREATING', 'IDENTITY_CREATED', 'ACCOUNT_COMMITTING');

CREATE INDEX registration_operations_repair_idx
    ON registration_operations (status, updated_at)
    WHERE status IN ('REPAIR_REQUIRED', 'COMPENSATION_PENDING');

ALTER TABLE sms_challenges
    ADD COLUMN registration_operation_id UUID REFERENCES registration_operations(id);

CREATE UNIQUE INDEX sms_challenges_registration_operation_idx
    ON sms_challenges (registration_operation_id)
    WHERE registration_operation_id IS NOT NULL;

ALTER TABLE registration_operations
    ADD CONSTRAINT registration_operations_sms_challenge_fk
    FOREIGN KEY (sms_challenge_id) REFERENCES sms_challenges(id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE wallet_provisioning_jobs (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    registration_operation_id UUID UNIQUE REFERENCES registration_operations(id),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'RETRY', 'LEASED', 'COMPLETED', 'REPAIR_REQUIRED')),
    policy_version TEXT NOT NULL,
    sla_expires_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    lease_until TIMESTAMPTZ,
    last_error_code TEXT,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX wallet_provisioning_jobs_ready_idx
    ON wallet_provisioning_jobs (next_attempt_at, updated_at)
    WHERE status IN ('PENDING', 'RETRY');

INSERT INTO wallet_provisioning_jobs(user_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at)
SELECT id,'PENDING','wallet-provisioning-v1',now()+interval '15 minutes',now(),now(),now()
FROM users WHERE status='PROVISIONING'
ON CONFLICT (user_id) DO NOTHING;

INSERT INTO permissions(code, scope, description)
VALUES ('platform.registrations.repair', 'PLATFORM', 'Inspect and retry recoverable registration operations.');

INSERT INTO role_permissions(role_code, permission_code) VALUES
    ('PLATFORM_SUPER_ADMIN', 'platform.registrations.repair'),
    ('PLATFORM_IDENTITY_ADMIN', 'platform.registrations.repair');
