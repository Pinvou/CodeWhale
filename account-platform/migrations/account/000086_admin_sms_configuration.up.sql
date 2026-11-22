CREATE TABLE sms_configuration_versions (
    revision BIGINT PRIMARY KEY CHECK (revision > 0),
    enabled BOOLEAN NOT NULL,
    provider_id TEXT NOT NULL CHECK (provider_id IN ('aliyun-managed','http-generated')),
    adapter_version TEXT NOT NULL CHECK (adapter_version = 'v1'),
    credential_profile TEXT NOT NULL CHECK (credential_profile IN ('aliyun-default','http-default')),
    sign_name TEXT,
    register_template TEXT,
    login_template TEXT,
    profile_mobile_template TEXT,
    password_reset_template TEXT,
    configuration_fingerprint TEXT NOT NULL CHECK (configuration_fingerprint ~ '^[0-9a-f]{64}$'),
    actor_user_id UUID NOT NULL REFERENCES users(id),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 16 AND 128),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    change_reason TEXT NOT NULL CHECK (length(change_reason) BETWEEN 10 AND 500),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (actor_user_id, idempotency_key),
    CHECK (
        (provider_id='aliyun-managed'
         AND credential_profile='aliyun-default'
         AND sign_name IS NOT NULL AND length(sign_name) BETWEEN 1 AND 100
         AND register_template IS NOT NULL AND length(register_template) BETWEEN 1 AND 120
         AND login_template IS NOT NULL AND length(login_template) BETWEEN 1 AND 120
         AND profile_mobile_template IS NOT NULL AND length(profile_mobile_template) BETWEEN 1 AND 120
         AND password_reset_template IS NOT NULL AND length(password_reset_template) BETWEEN 1 AND 120)
        OR
        (provider_id='http-generated'
         AND credential_profile='http-default'
         AND sign_name IS NULL AND register_template IS NULL AND login_template IS NULL
         AND profile_mobile_template IS NULL AND password_reset_template IS NULL)
    )
);

CREATE FUNCTION reject_sms_configuration_version_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'sms_configuration_versions is immutable'
        USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER sms_configuration_versions_reject_row_mutation
BEFORE UPDATE OR DELETE ON sms_configuration_versions
FOR EACH ROW
EXECUTE FUNCTION reject_sms_configuration_version_mutation();

CREATE TRIGGER sms_configuration_versions_reject_truncate
BEFORE TRUNCATE ON sms_configuration_versions
FOR EACH STATEMENT
EXECUTE FUNCTION reject_sms_configuration_version_mutation();

CREATE TABLE sms_configuration_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    active_revision BIGINT REFERENCES sms_configuration_versions(revision),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK ((revision=0)=(active_revision IS NULL)),
    CHECK (active_revision IS NULL OR active_revision=revision)
);

INSERT INTO sms_configuration_state(singleton,revision,active_revision)
VALUES(TRUE,0,NULL);

WITH new_permissions(code,description) AS (
    VALUES
      ('platform.sms-config.read','Read the redacted SMS delivery configuration and provider readiness.'),
      ('platform.sms-config.manage','Activate a versioned non-secret SMS delivery configuration.')
), inserted_permissions AS (
    INSERT INTO permissions(code,scope,description)
    SELECT code,'PLATFORM',description FROM new_permissions
    ON CONFLICT(code) DO UPDATE SET scope=EXCLUDED.scope,description=EXCLUDED.description
    RETURNING code
), desired(role_code,permission_code) AS (
    VALUES
      ('PLATFORM_SUPER_ADMIN','platform.sms-config.read'),
      ('PLATFORM_SUPER_ADMIN','platform.sms-config.manage'),
      ('PLATFORM_OPERATIONS_ADMIN','platform.sms-config.read'),
      ('PLATFORM_OPERATIONS_ADMIN','platform.sms-config.manage'),
      ('PLATFORM_AUDITOR','platform.sms-config.read')
), inserted_grants AS (
    INSERT INTO role_permissions(role_code,permission_code)
    SELECT role_code,permission_code FROM desired
    ON CONFLICT DO NOTHING
    RETURNING role_code
), changed_bindings AS (
    SELECT DISTINCT assignment.user_id,'PLATFORM'::text AS scope,
           '00000000-0000-0000-0000-000000000000'::uuid AS scope_id
    FROM role_assignments assignment
    JOIN inserted_grants ON inserted_grants.role_code=assignment.role_code
    WHERE assignment.revoked_at IS NULL
      AND assignment.starts_at<=now()
      AND (assignment.expires_at IS NULL OR assignment.expires_at>now())
)
SELECT increment_authorization_scope_version(user_id,scope,scope_id)
FROM changed_bindings;

COMMENT ON TABLE sms_configuration_versions IS
    'Immutable, non-secret SMS runtime configuration. Provider credentials and endpoints remain deployment-managed secrets.';
