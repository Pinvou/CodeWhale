ALTER TABLE users ADD COLUMN profile_version BIGINT NOT NULL DEFAULT 1 CHECK (profile_version > 0);

ALTER TABLE sms_challenges DROP CONSTRAINT sms_challenges_purpose_check;
ALTER TABLE sms_challenges ADD CONSTRAINT sms_challenges_purpose_check
    CHECK (purpose IN ('REGISTER', 'LOGIN', 'PROFILE_MOBILE', 'PASSWORD_RESET'));
ALTER TABLE slider_captcha_challenges DROP CONSTRAINT slider_captcha_challenges_purpose_check;
ALTER TABLE slider_captcha_challenges ADD CONSTRAINT slider_captcha_challenges_purpose_check
    CHECK (purpose IN ('REGISTER', 'LOGIN', 'PROFILE_MOBILE', 'PASSWORD_RESET'));

CREATE TABLE profile_change_requests (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id),
    identity_issuer TEXT NOT NULL,
    identity_subject TEXT NOT NULL,
    field TEXT NOT NULL CHECK (field IN ('USERNAME', 'MOBILE')),
    old_value TEXT NOT NULL,
    new_value TEXT NOT NULL,
    expected_profile_version BIGINT NOT NULL CHECK (expected_profile_version > 0),
    sms_challenge_id UUID REFERENCES sms_challenges(id),
    sms_verified_at TIMESTAMPTZ,
    identity_updated_at TIMESTAMPTZ,
    account_committed_at TIMESTAMPTZ,
    sessions_revoked_at TIMESTAMPTZ,
    status TEXT NOT NULL CHECK (status IN (
        'PENDING', 'VERIFIED', 'IDENTITY_UPDATED', 'ACCOUNT_COMMITTED',
        'RETRY', 'COMPLETED', 'REPAIR_REQUIRED', 'CANCELLED_ACCOUNT_CLOSURE'
    )),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    sla_expires_at TIMESTAMPTZ NOT NULL,
    last_error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (field = 'MOBILE' OR sms_challenge_id IS NULL),
    CHECK (field = 'MOBILE' OR sms_verified_at IS NULL)
);

CREATE UNIQUE INDEX profile_change_requests_active_field_idx
    ON profile_change_requests(user_id, field)
    WHERE status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED');
CREATE UNIQUE INDEX profile_change_requests_active_username_idx
    ON profile_change_requests(new_value)
    WHERE field='USERNAME' AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED');
CREATE UNIQUE INDEX profile_change_requests_active_mobile_idx
    ON profile_change_requests(new_value)
    WHERE field='MOBILE' AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED');
CREATE INDEX profile_change_requests_ready_idx ON profile_change_requests(next_attempt_at, updated_at)
    WHERE status IN ('VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY');

CREATE TABLE password_reset_operations (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id),
    identity_issuer TEXT NOT NULL,
    identity_subject TEXT NOT NULL,
    sms_challenge_id UUID NOT NULL UNIQUE REFERENCES sms_challenges(id),
    status TEXT NOT NULL CHECK (status IN ('VERIFIED','COMPLETED','FAILED')),
    last_error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ
);

INSERT INTO permissions(code,scope,description) VALUES
    ('platform.profile-changes.repair','PLATFORM','Inspect and retry recoverable profile changes.');
INSERT INTO role_permissions(role_code,permission_code) VALUES
    ('PLATFORM_SUPER_ADMIN','platform.profile-changes.repair'),
    ('PLATFORM_IDENTITY_ADMIN','platform.profile-changes.repair');
