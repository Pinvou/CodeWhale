CREATE TABLE password_preparation_authorizations (
    id uuid PRIMARY KEY,
    captcha_id uuid NOT NULL UNIQUE REFERENCES slider_captcha_challenges(id),
    mobile_e164 text NOT NULL,
    purpose text NOT NULL CHECK (purpose IN ('REGISTER','PASSWORD_RESET')),
    source_digest bytea NOT NULL CHECK (octet_length(source_digest)=32),
    device_digest bytea NOT NULL CHECK (octet_length(device_digest)=32),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
    expires_at timestamptz NOT NULL,
    identity_incarnation uuid,
    sms_consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
GRANT SELECT,INSERT,UPDATE ON password_preparation_authorizations TO account_service;
