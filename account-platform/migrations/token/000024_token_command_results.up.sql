CREATE TABLE token_command_results (
    command_reference UUID PRIMARY KEY,
    command_type TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version>0),
    actor_user_id UUID NOT NULL,
    actor_permission TEXT NOT NULL,
    authorization_version BIGINT NOT NULL CHECK (authorization_version>0),
    idempotency_key_digest TEXT NOT NULL CHECK (idempotency_key_digest ~ '^[0-9a-f]{64}$'),
    idempotency_fingerprint TEXT NOT NULL CHECK (idempotency_fingerprint ~ '^[0-9a-f]{64}$'),
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    target_resource_type TEXT NOT NULL,
    target_resource_id TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 10 AND 2000),
    expected_versions JSONB NOT NULL CHECK (jsonb_typeof(expected_versions)='object'),
    precheck_fingerprint TEXT NOT NULL CHECK (precheck_fingerprint ~ '^[0-9a-f]{64}$'),
    approval_request_id UUID,
    approval_command_fingerprint TEXT CHECK (approval_command_fingerprint IS NULL OR approval_command_fingerprint ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK (status IN ('PENDING','SUCCEEDED','REJECTED','FAILED','UNKNOWN')),
    before_state JSONB CHECK (before_state IS NULL OR jsonb_typeof(before_state)='object'),
    after_state JSONB CHECK (after_state IS NULL OR jsonb_typeof(after_state)='object'),
    redacted_result JSONB CHECK (redacted_result IS NULL OR jsonb_typeof(redacted_result)='object'),
    stable_error_code TEXT CHECK (
        stable_error_code IS NULL OR (
            char_length(stable_error_code) BETWEEN 1 AND 120
            AND stable_error_code ~ '^[A-Z][A-Z0-9_]*$'
            AND stable_error_code !~ '(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)'
        )
    ),
    audit_request_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    completed_at TIMESTAMPTZ,
    UNIQUE(actor_user_id,command_type,idempotency_key_digest),
    CHECK ((approval_request_id IS NULL)=(approval_command_fingerprint IS NULL)),
    CHECK ((status IN ('PENDING','SUCCEEDED'))=(stable_error_code IS NULL)),
    CHECK ((status='PENDING')=(completed_at IS NULL))
);

CREATE INDEX token_command_results_recovery_idx
    ON token_command_results(actor_user_id,command_type,idempotency_fingerprint,created_at DESC);
CREATE INDEX token_command_results_target_idx
    ON token_command_results(target_resource_type,target_resource_id,created_at DESC);
CREATE UNIQUE INDEX token_command_results_approval_idx
    ON token_command_results(approval_request_id,command_type)
    WHERE approval_request_id IS NOT NULL;

CREATE FUNCTION protect_token_command_result_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.command_reference IS DISTINCT FROM OLD.command_reference
       OR NEW.command_type IS DISTINCT FROM OLD.command_type
       OR NEW.policy_version IS DISTINCT FROM OLD.policy_version
       OR NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id
       OR NEW.actor_permission IS DISTINCT FROM OLD.actor_permission
       OR NEW.authorization_version IS DISTINCT FROM OLD.authorization_version
       OR NEW.idempotency_key_digest IS DISTINCT FROM OLD.idempotency_key_digest
       OR NEW.idempotency_fingerprint IS DISTINCT FROM OLD.idempotency_fingerprint
       OR NEW.request_digest IS DISTINCT FROM OLD.request_digest
       OR NEW.target_resource_type IS DISTINCT FROM OLD.target_resource_type
       OR NEW.target_resource_id IS DISTINCT FROM OLD.target_resource_id
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.expected_versions IS DISTINCT FROM OLD.expected_versions
       OR NEW.precheck_fingerprint IS DISTINCT FROM OLD.precheck_fingerprint
       OR NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id
       OR NEW.approval_command_fingerprint IS DISTINCT FROM OLD.approval_command_fingerprint
       OR NEW.before_state IS DISTINCT FROM OLD.before_state
       OR NEW.audit_request_id IS DISTINCT FROM OLD.audit_request_id
       OR OLD.status<>'PENDING' THEN
        RAISE EXCEPTION 'token command identity/result is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER token_command_results_immutable
BEFORE UPDATE ON token_command_results
FOR EACH ROW EXECUTE FUNCTION protect_token_command_result_identity();

CREATE FUNCTION reject_token_command_result_removal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'token command results are retained' USING ERRCODE='55000';
END;
$$;
CREATE TRIGGER token_command_results_no_delete
BEFORE DELETE OR TRUNCATE ON token_command_results
FOR EACH STATEMENT EXECUTE FUNCTION reject_token_command_result_removal();
