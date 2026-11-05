ALTER TABLE users
    ADD COLUMN mobile_e164 TEXT,
    ADD COLUMN mobile_verified_at TIMESTAMPTZ;

CREATE UNIQUE INDEX users_mobile_e164_unique_idx
    ON users (mobile_e164)
    WHERE mobile_e164 IS NOT NULL AND status <> 'DELETED';

CREATE TABLE sms_challenges (
    id UUID PRIMARY KEY,
    mobile_e164 TEXT NOT NULL,
    purpose TEXT NOT NULL CHECK (purpose IN ('REGISTER', 'LOGIN')),
    code_hash BYTEA NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('CREATED', 'SENT', 'CONSUMED', 'FAILED', 'EXPIRED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0 AND attempts <= 5),
    provider_message_id TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX sms_challenges_mobile_rate_idx
    ON sms_challenges (mobile_e164, purpose, created_at DESC);

CREATE INDEX sms_challenges_expiry_idx
    ON sms_challenges (expires_at)
    WHERE status IN ('CREATED', 'SENT');

CREATE TABLE mall_user_sync_jobs (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    mobile_e164 TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'RETRY', 'LEASED', 'SYNCED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    buyer_id TEXT,
    synced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX mall_user_sync_jobs_ready_idx
    ON mall_user_sync_jobs (next_attempt_at, updated_at)
    WHERE status IN ('PENDING', 'RETRY');
