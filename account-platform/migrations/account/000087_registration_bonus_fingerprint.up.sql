SET ROLE account_service;

CREATE TABLE registration_bonus_eligibility_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','READY','BLOCKED')),
    fingerprint_key_id TEXT,
    fingerprint_key_verifier BYTEA CHECK (fingerprint_key_verifier IS NULL OR octet_length(fingerprint_key_verifier) = 32),
    recoverable_registration_count BIGINT NOT NULL DEFAULT 0 CHECK (recoverable_registration_count >= 0),
    unrecoverable_registration_count BIGINT NOT NULL DEFAULT 0 CHECK (unrecoverable_registration_count >= 0),
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status = 'PENDING') = (completed_at IS NULL)),
    CHECK ((status = 'PENDING') = (fingerprint_key_id IS NULL AND fingerprint_key_verifier IS NULL)),
    CHECK ((status = 'READY') IS NOT TRUE OR unrecoverable_registration_count = 0),
    CHECK ((status = 'BLOCKED') IS NOT TRUE OR unrecoverable_registration_count > 0)
);
INSERT INTO registration_bonus_eligibility_state(singleton) VALUES(TRUE);

CREATE TABLE registration_bonus_mobile_eligibility (
    mobile_fingerprint BYTEA PRIMARY KEY CHECK (octet_length(mobile_fingerprint) = 32),
    fingerprint_key_id TEXT NOT NULL CHECK (length(fingerprint_key_id) BETWEEN 1 AND 100),
    first_user_id UUID NOT NULL REFERENCES users(id),
    source TEXT NOT NULL CHECK (source IN ('HISTORICAL_BACKFILL','REGISTRATION')),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE FUNCTION reject_registration_bonus_eligibility_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'registration bonus mobile eligibility is permanent and immutable';
END $$;
CREATE TRIGGER registration_bonus_mobile_eligibility_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON registration_bonus_mobile_eligibility
FOR EACH STATEMENT EXECUTE FUNCTION reject_registration_bonus_eligibility_mutation();

CREATE FUNCTION protect_registration_bonus_eligibility_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.status = 'PENDING'
       AND NEW.status IN ('READY','BLOCKED')
       AND NEW.singleton = OLD.singleton
       AND NEW.fingerprint_key_id IS NOT NULL
       AND NEW.fingerprint_key_verifier IS NOT NULL
       AND NEW.completed_at IS NOT NULL THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'registration bonus eligibility readiness is append-only and can transition only once';
END $$;
CREATE TRIGGER registration_bonus_eligibility_state_progression
BEFORE UPDATE OR DELETE ON registration_bonus_eligibility_state
FOR EACH ROW EXECUTE FUNCTION protect_registration_bonus_eligibility_state();
CREATE TRIGGER registration_bonus_eligibility_state_no_truncate
BEFORE TRUNCATE ON registration_bonus_eligibility_state
FOR EACH STATEMENT EXECUTE FUNCTION protect_registration_bonus_eligibility_state();

CREATE FUNCTION require_registration_bonus_capable_writer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.mobile_verified_at IS NOT NULL
       AND NEW.mobile_e164 LIKE '+86%'
       AND current_setting('pinvou.registration_bonus_capable', true) IS DISTINCT FROM '1' THEN
        RAISE EXCEPTION 'mobile registration requires a registration-bonus-capable Account writer';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER users_registration_bonus_capable_writer
BEFORE INSERT ON users
FOR EACH ROW EXECUTE FUNCTION require_registration_bonus_capable_writer();

ALTER TABLE wallet_provisioning_jobs
    ADD COLUMN registration_bonus_mode TEXT NOT NULL DEFAULT 'NOT_APPLICABLE'
        CHECK (registration_bonus_mode IN ('NOT_APPLICABLE','EVALUATE')),
    ADD COLUMN registration_bonus_fingerprint_key_id TEXT,
    ADD COLUMN registration_bonus_fingerprint BYTEA,
    ADD CONSTRAINT wallet_provisioning_registration_bonus_fingerprint_check CHECK (
        (registration_bonus_mode = 'NOT_APPLICABLE' AND registration_bonus_fingerprint_key_id IS NULL AND registration_bonus_fingerprint IS NULL)
        OR
        (registration_bonus_mode = 'EVALUATE' AND registration_operation_id IS NOT NULL AND length(registration_bonus_fingerprint_key_id) BETWEEN 1 AND 100 AND octet_length(registration_bonus_fingerprint) = 32)
    );

INSERT INTO approval_command_policies(
    command_type,policy_version,approval_mode,requester_permission,
    risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,
    critical_resource_fields,expires_after,execution_window,executor_identity,created_at
) VALUES (
    'BILLING_REGISTRATION_BONUS_POLICY_UPDATE',1,'AUDIT_ONLY','platform.registration-bonus.manage',
    'GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',
    INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()
);

WITH new_permissions(code,description) AS (
    VALUES
      ('platform.registration-bonus.read','Read the active new-registration bonus policy.'),
      ('platform.registration-bonus.manage','Activate a versioned new-registration bonus policy.')
), inserted_permissions AS (
    INSERT INTO permissions(code,scope,description)
    SELECT code,'PLATFORM',description FROM new_permissions
    ON CONFLICT(code) DO UPDATE SET scope=EXCLUDED.scope,description=EXCLUDED.description
    RETURNING code
), desired(role_code,permission_code) AS (
    VALUES
      ('PLATFORM_SUPER_ADMIN','platform.registration-bonus.read'),
      ('PLATFORM_SUPER_ADMIN','platform.registration-bonus.manage'),
      ('PLATFORM_OPERATIONS_ADMIN','platform.registration-bonus.read'),
      ('PLATFORM_OPERATIONS_ADMIN','platform.registration-bonus.manage'),
      ('PLATFORM_AUDITOR','platform.registration-bonus.read')
), inserted_grants AS (
    INSERT INTO role_permissions(role_code,permission_code)
    SELECT role_code,permission_code FROM desired ON CONFLICT DO NOTHING RETURNING role_code
), changed_bindings AS (
    SELECT DISTINCT assignment.user_id,'PLATFORM'::text AS scope,
           '00000000-0000-0000-0000-000000000000'::uuid AS scope_id
    FROM role_assignments assignment JOIN inserted_grants ON inserted_grants.role_code=assignment.role_code
    WHERE assignment.revoked_at IS NULL AND assignment.starts_at<=now()
      AND (assignment.expires_at IS NULL OR assignment.expires_at>now())
)
SELECT increment_authorization_scope_version(user_id,scope,scope_id) FROM changed_bindings;

COMMENT ON COLUMN wallet_provisioning_jobs.registration_bonus_fingerprint IS
    'Keyed pseudonymous HMAC-SHA-256 of the normalized registration mobile, frozen while provisioning is pending and erased after completion. Raw mobile data is never copied into the provisioning job.';
COMMENT ON TABLE registration_bonus_eligibility_state IS
    'Fail-closed readiness gate. BLOCKED means a historical successful mobile registration was already anonymized and cannot be safely reconstructed.';
COMMENT ON TABLE registration_bonus_mobile_eligibility IS
    'Permanent Account-side first-registration authority. Rows survive account closure and prevent the same mobile from becoming eligible again.';
COMMENT ON TRIGGER users_registration_bonus_capable_writer ON users IS
    'Rolling-upgrade fence: Account binaries predating eligibility writes cannot commit a verified mobile registration after this migration.';
