ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('PROVISIONING','ACTIVE','DISABLED','DELETED','REPAIR_REQUIRED','BOOTSTRAP_PENDING','CLOSING'));

INSERT INTO permissions(code,scope,description) VALUES
('platform.account-closures.read','PLATFORM','Read redacted account closure state and blockers.'),
('platform.account-closures.repair','PLATFORM','Retry a failed account closure stage without bypassing blockers.');
INSERT INTO role_permissions(role_code,permission_code) VALUES
('PLATFORM_IDENTITY_ADMIN','platform.account-closures.read'),
('PLATFORM_IDENTITY_ADMIN','platform.account-closures.repair');

CREATE TABLE account_closure_requests (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL UNIQUE REFERENCES users(id),
    idempotency_key_digest TEXT NOT NULL CHECK (idempotency_key_digest ~ '^[0-9a-f]{64}$'),
    request_fingerprint TEXT NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    status TEXT NOT NULL CHECK (status IN ('IN_PROGRESS','BLOCKED','RETRY','REPAIR_REQUIRED','COMPLETED')),
    stage TEXT NOT NULL CHECK (stage IN ('CLOSURE_REQUESTED','QUIESCING','FINANCIAL_CLEARANCE','IDENTITY_CLEANUP','ANONYMIZED','COMPLETED')),
    policy_version INTEGER NOT NULL DEFAULT 1 CHECK (policy_version>0),
    blocker_snapshot JSONB NOT NULL DEFAULT '[]'::jsonb,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts>=0),
    last_error_code TEXT,
    next_attempt_at TIMESTAMPTZ,
    lease_until TIMESTAMPTZ,
    reauthenticated_at TIMESTAMPTZ NOT NULL,
    sla_expires_at TIMESTAMPTZ NOT NULL,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    UNIQUE(user_id,idempotency_key_digest),
    CHECK ((status='COMPLETED')=(completed_at IS NOT NULL)),
    CHECK (sla_expires_at>created_at)
);
CREATE INDEX account_closure_claim_idx ON account_closure_requests(next_attempt_at,id)
    WHERE status IN ('IN_PROGRESS','RETRY','BLOCKED') AND stage IN ('CLOSURE_REQUESTED','QUIESCING','FINANCIAL_CLEARANCE');

CREATE TABLE account_closure_commands (
    id UUID PRIMARY KEY,
    closure_request_id UUID NOT NULL REFERENCES account_closure_requests(id),
    actor_user_id UUID NOT NULL REFERENCES users(id),
    idempotency_key_digest TEXT NOT NULL CHECK (idempotency_key_digest ~ '^[0-9a-f]{64}$'),
    request_fingerprint TEXT NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    command_type TEXT NOT NULL CHECK (command_type='RETRY_STAGE'),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(actor_user_id,idempotency_key_digest)
);

CREATE TABLE account_closure_audit (
    id UUID PRIMARY KEY,
    closure_request_id UUID NOT NULL REFERENCES account_closure_requests(id),
    event_type TEXT NOT NULL,
    actor_user_id UUID,
    from_stage TEXT,
    to_stage TEXT,
    redacted_details JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX account_closure_audit_request_idx ON account_closure_audit(closure_request_id,occurred_at,id);

CREATE FUNCTION reject_account_closure_evidence_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'account closure command and audit evidence is immutable';
END $$;
CREATE TRIGGER account_closure_commands_immutable BEFORE UPDATE OR DELETE ON account_closure_commands
FOR EACH ROW EXECUTE FUNCTION reject_account_closure_evidence_mutation();
CREATE TRIGGER account_closure_audit_immutable BEFORE UPDATE OR DELETE ON account_closure_audit
FOR EACH ROW EXECUTE FUNCTION reject_account_closure_evidence_mutation();

CREATE FUNCTION enforce_account_closure_stage_progression() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE old_rank INTEGER; new_rank INTEGER;
BEGIN
    old_rank := array_position(ARRAY['CLOSURE_REQUESTED','QUIESCING','FINANCIAL_CLEARANCE','IDENTITY_CLEANUP','ANONYMIZED','COMPLETED'],OLD.stage);
    new_rank := array_position(ARRAY['CLOSURE_REQUESTED','QUIESCING','FINANCIAL_CLEARANCE','IDENTITY_CLEANUP','ANONYMIZED','COMPLETED'],NEW.stage);
    IF new_rank < old_rank OR NEW.user_id<>OLD.user_id OR NEW.idempotency_key_digest<>OLD.idempotency_key_digest OR NEW.request_fingerprint<>OLD.request_fingerprint OR NEW.policy_version<>OLD.policy_version OR NEW.created_at<>OLD.created_at THEN
        RAISE EXCEPTION 'account closure stage or frozen facts cannot move backward';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER account_closure_stage_guard BEFORE UPDATE ON account_closure_requests
FOR EACH ROW EXECUTE FUNCTION enforce_account_closure_stage_progression();

INSERT INTO approval_command_policies(
    command_type,policy_version,approval_mode,requester_permission,risk_class,
    approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,
    expires_after,execution_window,executor_identity,created_at
) VALUES (
    'ACCOUNT_CLOSURE_RETRY',1,'AUDIT_ONLY','platform.account-closures.repair','GOVERNANCE_OR_NORMAL_STATE_MACHINE',
    0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()
);
