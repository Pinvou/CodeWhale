ALTER TABLE sms_challenges
    ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'aliyun-managed',
    ADD COLUMN adapter_version TEXT NOT NULL DEFAULT 'v1';
ALTER TABLE sms_challenges ALTER COLUMN provider_id DROP DEFAULT;
ALTER TABLE sms_challenges ALTER COLUMN adapter_version DROP DEFAULT;

CREATE TABLE sms_send_attempts (
    id UUID PRIMARY KEY,
    challenge_id UUID REFERENCES sms_challenges(id) ON DELETE SET NULL,
    mobile_hash BYTEA NOT NULL CHECK (octet_length(mobile_hash)=32),
    source_hash BYTEA NOT NULL CHECK (octet_length(source_hash)=32),
    device_hash BYTEA NOT NULL CHECK (octet_length(device_hash)=32),
    purpose TEXT NOT NULL CHECK (purpose IN ('REGISTER','LOGIN')),
    result TEXT NOT NULL CHECK (result IN ('ACCEPTED','RATE_LIMITED')),
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX sms_send_attempts_source_idx ON sms_send_attempts(source_hash, occurred_at DESC);
CREATE INDEX sms_send_attempts_device_idx ON sms_send_attempts(device_hash, occurred_at DESC);
CREATE INDEX sms_send_attempts_mobile_idx ON sms_send_attempts(mobile_hash, occurred_at DESC);

COMMENT ON TABLE sms_send_attempts IS 'Security audit and multidimensional limits for SMS sends; identifiers are one-way hashed.';
