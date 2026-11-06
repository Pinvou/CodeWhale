ALTER TABLE sms_send_attempts
    ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN adapter_version TEXT NOT NULL DEFAULT 'legacy';

CREATE INDEX sms_send_attempts_provider_idx ON sms_send_attempts(provider_id, purpose, occurred_at DESC);
CREATE INDEX sms_send_attempts_purpose_mobile_idx ON sms_send_attempts(purpose, mobile_hash, occurred_at DESC);

CREATE TABLE sms_verification_attempts (
    id UUID PRIMARY KEY,
    challenge_id UUID REFERENCES sms_challenges(id) ON DELETE SET NULL,
    mobile_hash BYTEA NOT NULL CHECK (octet_length(mobile_hash)=32),
    source_hash BYTEA NOT NULL CHECK (octet_length(source_hash)=32),
    device_hash BYTEA NOT NULL CHECK (octet_length(device_hash)=32),
    purpose TEXT NOT NULL CHECK (purpose IN ('REGISTER','LOGIN','PROFILE_MOBILE','PASSWORD_RESET')),
    provider_id TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('ACCEPTED','RATE_LIMITED')),
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX sms_verification_attempts_source_idx ON sms_verification_attempts(source_hash, occurred_at DESC);
CREATE INDEX sms_verification_attempts_device_idx ON sms_verification_attempts(device_hash, occurred_at DESC);
CREATE INDEX sms_verification_attempts_mobile_idx ON sms_verification_attempts(mobile_hash, occurred_at DESC);
CREATE INDEX sms_verification_attempts_provider_idx ON sms_verification_attempts(provider_id, purpose, occurred_at DESC);

COMMENT ON TABLE sms_verification_attempts IS 'One-way risk dimensions for SMS verification admission; contains no code or mobile plaintext.';
