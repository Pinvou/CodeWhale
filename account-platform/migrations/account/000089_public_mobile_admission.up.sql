CREATE TABLE mobile_public_admission_buckets (
    scope TEXT NOT NULL CHECK (scope IN ('GLOBAL','SOURCE','PRECHECK_MOBILE','CAPTCHA_MOBILE')),
    bucket_hash BYTEA NOT NULL CHECK (octet_length(bucket_hash)=32),
    attempts INTEGER NOT NULL CHECK (attempts>0 AND attempts<=300),
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope,bucket_hash)
);
CREATE INDEX mobile_public_admission_expiry_idx ON mobile_public_admission_buckets(expires_at);
