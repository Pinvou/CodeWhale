CREATE TABLE approval_command_policies (
    command_type TEXT NOT NULL,
    policy_version BIGINT NOT NULL CHECK (policy_version > 0),
    approval_mode TEXT NOT NULL CHECK (approval_mode IN ('APPROVAL_REQUIRED','AUDIT_ONLY')),
    requester_permission TEXT NOT NULL,
    approver_permission TEXT,
    risk_class TEXT NOT NULL,
    approval_count INTEGER NOT NULL CHECK (approval_count IN (0,1)),
    self_approval_forbidden BOOLEAN NOT NULL,
    snapshot_schema_version BIGINT NOT NULL CHECK (snapshot_schema_version > 0),
    critical_resource_fields JSONB NOT NULL CHECK (jsonb_typeof(critical_resource_fields)='array'),
    expires_after INTERVAL NOT NULL CHECK (expires_after>INTERVAL '0' AND expires_after<=INTERVAL '72 hours'),
    execution_window INTERVAL NOT NULL CHECK (execution_window>INTERVAL '0' AND execution_window<=INTERVAL '1 hour'),
    executor_identity TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(command_type,policy_version),
    CHECK ((approval_mode='APPROVAL_REQUIRED')=(approval_count=1)),
    CHECK ((approval_mode='APPROVAL_REQUIRED')=(approver_permission IS NOT NULL)),
    CHECK ((approval_mode='APPROVAL_REQUIRED')=self_approval_forbidden)
);

CREATE TABLE approval_requests (
    id UUID PRIMARY KEY,
    command_type TEXT NOT NULL,
    policy_version BIGINT NOT NULL,
    requester_user_id UUID NOT NULL REFERENCES users(id),
    requester_binding_id UUID NOT NULL REFERENCES role_assignments(id),
    requester_authorization_version BIGINT NOT NULL CHECK (requester_authorization_version>0),
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 120),
    normalized_command JSONB NOT NULL CHECK (jsonb_typeof(normalized_command)='object'),
    command_fingerprint BYTEA NOT NULL CHECK (octet_length(command_fingerprint)=32),
    target_resource_type TEXT NOT NULL,
    target_resource_id TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 10 AND 2000),
    redacted_evidence JSONB NOT NULL CHECK (jsonb_typeof(redacted_evidence) IN ('object','array')),
    expected_impact JSONB NOT NULL CHECK (jsonb_typeof(expected_impact)='object'),
    status TEXT NOT NULL CHECK (status IN ('DRAFT','SUBMITTED','APPROVED','EXECUTING','EXECUTED','EXECUTION_FAILED','REJECTED','WITHDRAWN','EXPIRED','INVALIDATED')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
    supersedes_request_id UUID REFERENCES approval_requests(id),
    expires_at TIMESTAMPTZ NOT NULL,
    execution_deadline TIMESTAMPTZ,
    invalidation_code TEXT,
    invalidation_details JSONB,
    submitted_at TIMESTAMPTZ,
    decided_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY(command_type,policy_version) REFERENCES approval_command_policies(command_type,policy_version),
    UNIQUE(requester_user_id,command_type,idempotency_key),
    CHECK (status<>'DRAFT' OR submitted_at IS NULL),
    CHECK ((status IN ('SUBMITTED','APPROVED','EXECUTING','EXECUTED','EXECUTION_FAILED','REJECTED','WITHDRAWN')) IS NOT TRUE OR submitted_at IS NOT NULL),
    CHECK ((status IN ('APPROVED','EXECUTING','EXECUTED','EXECUTION_FAILED')) IS NOT TRUE OR execution_deadline IS NOT NULL)
);

CREATE INDEX approval_requests_queue_idx ON approval_requests(status,expires_at,updated_at,id);
CREATE INDEX approval_requests_target_idx ON approval_requests(target_resource_type,target_resource_id,created_at DESC);

CREATE TABLE approval_request_snapshots (
    approval_request_id UUID PRIMARY KEY REFERENCES approval_requests(id),
    snapshot_schema_version BIGINT NOT NULL,
    critical_resource_versions JSONB NOT NULL CHECK (jsonb_typeof(critical_resource_versions)='object'),
    financial_snapshot JSONB NOT NULL CHECK (jsonb_typeof(financial_snapshot)='object'),
    policy_snapshot JSONB NOT NULL CHECK (jsonb_typeof(policy_snapshot)='object'),
    requester_authorization_snapshot JSONB NOT NULL CHECK (jsonb_typeof(requester_authorization_snapshot)='object'),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE approval_decisions (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL UNIQUE REFERENCES approval_requests(id),
    approver_user_id UUID NOT NULL REFERENCES users(id),
    approver_binding_id UUID NOT NULL REFERENCES role_assignments(id),
    approver_authorization_version BIGINT NOT NULL CHECK (approver_authorization_version>0),
    decision TEXT NOT NULL CHECK (decision IN ('APPROVED','REJECTED')),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 3 AND 2000),
    authorization_snapshot JSONB NOT NULL CHECK (jsonb_typeof(authorization_snapshot)='object'),
    decided_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE approval_state_events (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL REFERENCES approval_requests(id),
    from_status TEXT,
    to_status TEXT NOT NULL,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('USER','SERVICE','SYSTEM')),
    actor_id TEXT NOT NULL,
    reason_code TEXT,
    details JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX approval_state_events_request_idx ON approval_state_events(approval_request_id,occurred_at,id);

CREATE TABLE approval_executions (
    id UUID PRIMARY KEY,
    approval_request_id UUID NOT NULL REFERENCES approval_requests(id),
    command_type TEXT NOT NULL,
    command_fingerprint BYTEA NOT NULL CHECK (octet_length(command_fingerprint)=32),
    execution_idempotency_key TEXT NOT NULL UNIQUE,
    executor_identity TEXT NOT NULL,
    job_run_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('EXECUTING','EXECUTED','FAILED')),
    authorization_snapshot JSONB NOT NULL,
    domain_result JSONB,
    ledger_references JSONB,
    error_class TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    UNIQUE(approval_request_id,command_type),
    FOREIGN KEY(approval_request_id) REFERENCES approval_requests(id)
);

CREATE FUNCTION protect_approval_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'approval command policies are append-only' USING ERRCODE='55000';
END;
$$;
CREATE TRIGGER approval_command_policies_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_command_policies FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();

CREATE FUNCTION protect_submitted_approval_request() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
		(OLD.status='DRAFT' AND NEW.status='SUBMITTED')
		OR (OLD.status='SUBMITTED' AND NEW.status IN ('APPROVED','REJECTED','WITHDRAWN','EXPIRED','INVALIDATED'))
		OR (OLD.status='APPROVED' AND NEW.status IN ('EXECUTING','INVALIDATED'))
		OR (OLD.status='EXECUTING' AND NEW.status IN ('EXECUTED','EXECUTION_FAILED'))
		OR (OLD.status='EXECUTION_FAILED' AND NEW.status IN ('EXECUTING','INVALIDATED'))
	) THEN
		RAISE EXCEPTION 'invalid approval request state transition' USING ERRCODE='23514';
	END IF;
    IF (OLD.status<>'DRAFT' OR NEW.status<>'DRAFT') AND (
       NEW.command_type IS DISTINCT FROM OLD.command_type OR NEW.policy_version IS DISTINCT FROM OLD.policy_version
       OR NEW.requester_user_id IS DISTINCT FROM OLD.requester_user_id OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key OR NEW.normalized_command IS DISTINCT FROM OLD.normalized_command
       OR NEW.command_fingerprint IS DISTINCT FROM OLD.command_fingerprint OR NEW.target_resource_type IS DISTINCT FROM OLD.target_resource_type
       OR NEW.target_resource_id IS DISTINCT FROM OLD.target_resource_id OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.redacted_evidence IS DISTINCT FROM OLD.redacted_evidence OR NEW.expected_impact IS DISTINCT FROM OLD.expected_impact
       OR NEW.supersedes_request_id IS DISTINCT FROM OLD.supersedes_request_id
    ) THEN
        RAISE EXCEPTION 'submitted approval command is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER approval_requests_command_immutable BEFORE UPDATE ON approval_requests FOR EACH ROW EXECUTE FUNCTION protect_submitted_approval_request();
CREATE TRIGGER approval_requests_no_delete BEFORE DELETE OR TRUNCATE ON approval_requests FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();

CREATE FUNCTION protect_submitted_approval_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS(SELECT 1 FROM approval_requests WHERE id=OLD.approval_request_id AND status='DRAFT') THEN
        RAISE EXCEPTION 'submitted approval snapshot is immutable' USING ERRCODE='55000';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER approval_request_snapshots_immutable BEFORE UPDATE ON approval_request_snapshots FOR EACH ROW EXECUTE FUNCTION protect_submitted_approval_snapshot();
CREATE TRIGGER approval_request_snapshots_no_delete BEFORE DELETE OR TRUNCATE ON approval_request_snapshots FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();
CREATE FUNCTION validate_approval_decision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
	requester UUID;
	request_status TEXT;
BEGIN
	SELECT requester_user_id,status INTO requester,request_status FROM approval_requests WHERE id=NEW.approval_request_id FOR UPDATE;
	IF requester IS NULL OR requester=NEW.approver_user_id THEN
		RAISE EXCEPTION 'approval requester cannot decide their own request' USING ERRCODE='23514';
	END IF;
	IF request_status<>'SUBMITTED' THEN
		RAISE EXCEPTION 'approval request is not submitted' USING ERRCODE='23514';
	END IF;
	RETURN NEW;
END;
$$;
CREATE TRIGGER approval_decisions_validate BEFORE INSERT ON approval_decisions FOR EACH ROW EXECUTE FUNCTION validate_approval_decision();
CREATE TRIGGER approval_decisions_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_decisions FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();
CREATE TRIGGER approval_state_events_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON approval_state_events FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();

CREATE FUNCTION protect_approval_execution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
		(OLD.status='EXECUTING' AND NEW.status IN ('EXECUTED','FAILED'))
		OR (OLD.status='FAILED' AND NEW.status='EXECUTING')
	) THEN
		RAISE EXCEPTION 'invalid approval execution state transition' USING ERRCODE='23514';
	END IF;
	IF NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id
		OR NEW.command_type IS DISTINCT FROM OLD.command_type
		OR NEW.command_fingerprint IS DISTINCT FROM OLD.command_fingerprint
		OR NEW.execution_idempotency_key IS DISTINCT FROM OLD.execution_idempotency_key
		OR NEW.executor_identity IS DISTINCT FROM OLD.executor_identity THEN
		RAISE EXCEPTION 'approval execution identity is immutable' USING ERRCODE='55000';
	END IF;
	RETURN NEW;
END;
$$;
CREATE TRIGGER approval_executions_identity_immutable BEFORE UPDATE ON approval_executions FOR EACH ROW EXECUTE FUNCTION protect_approval_execution();
CREATE TRIGGER approval_executions_no_delete BEFORE DELETE OR TRUNCATE ON approval_executions FOR EACH STATEMENT EXECUTE FUNCTION protect_approval_policy();

INSERT INTO approval_command_policies(command_type,policy_version,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at)
SELECT command_type,1,'APPROVAL_REQUIRED','platform.billing.repairs.request','platform.billing.repairs.approve','FINANCIAL_FACT_CHANGE',1,true,1,'["target_id","resource_version","financial_snapshot","policy_version"]'::jsonb,INTERVAL '24 hours',INTERVAL '5 minutes','billing-command-executor',now()
FROM unnest(ARRAY['WALLET_ADJUSTMENT','ENTITLEMENT_COMPONENT_ADJUSTMENT','LEDGER_REPAIR','RECEIVABLE_WRITE_OFF','PAYMENT_FINANCIAL_REPAIR','REFUND_EXCEPTION_REPAIR','CHARGEBACK_FINANCIAL_REPAIR','RECONCILIATION_FINANCIAL_REPAIR','USAGE_SETTLEMENT_REPAIR']) AS command_type;

INSERT INTO approval_command_policies(command_type,policy_version,approval_mode,requester_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at) VALUES
('ACCOUNT_ROLE_BINDING_GRANT',1,'AUDIT_ONLY','platform.roles.assign','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_ROLE_BINDING_REVOKE',1,'AUDIT_ONLY','platform.roles.assign','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_USER_STATUS_CHANGE',1,'AUDIT_ONLY','platform.users.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_USER_DELETE',1,'AUDIT_ONLY','platform.users.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_ORGANIZATION_CREATE',1,'AUDIT_ONLY','platform.organizations.create','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_ORGANIZATION_RENAME',1,'AUDIT_ONLY','platform.organizations.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_ORGANIZATION_STATUS_CHANGE',1,'AUDIT_ONLY','platform.organizations.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_ORGANIZATION_DELETE',1,'AUDIT_ONLY','platform.organizations.delete','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_MEMBER_ADD',1,'AUDIT_ONLY','platform.organizations.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_MEMBER_ROLES_REPLACE',1,'AUDIT_ONLY','platform.organizations.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_MEMBER_REMOVE',1,'AUDIT_ONLY','platform.organizations.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_OWNERSHIP_TRANSFER',1,'AUDIT_ONLY','platform.organizations.owner.transfer','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_UNIT_CREATE',1,'AUDIT_ONLY','platform.organizations.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_UNIT_RENAME',1,'AUDIT_ONLY','platform.organizations.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_UNIT_DELETE',1,'AUDIT_ONLY','platform.organizations.status.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_REGISTRATION_RETRY',1,'AUDIT_ONLY','platform.registrations.repair','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_REGISTRATION_COMPENSATE',1,'AUDIT_ONLY','platform.registrations.repair','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_PROFILE_CHANGE_RETRY',1,'AUDIT_ONLY','platform.profile-changes.repair','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_PRICE_VERSION_CREATE',1,'AUDIT_ONLY','platform.pricing.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_PAYMENT_ORDER_SYNC',1,'AUDIT_ONLY','platform.billing.orders.sync','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_PAYMENT_ORDER_EVIDENCE_REPAIR',1,'AUDIT_ONLY','platform.billing.repairs.request','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_NORMAL_REFUND',1,'AUDIT_ONLY','platform.billing.repairs.request','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_ADJUSTMENT_REQUEST',1,'AUDIT_ONLY','platform.billing.repairs.request','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_ADJUSTMENT_REJECT',1,'AUDIT_ONLY','platform.billing.adjustments.approve','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_USAGE_HOLD_SAFE_RELEASE',1,'AUDIT_ONLY','platform.billing.holds.release','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_RECONCILIATION_RUN',1,'AUDIT_ONLY','platform.usage.reconcile.retry','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_ACCOUNT_STATUS_CHANGE',1,'AUDIT_ONLY','platform.tokens.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_ROUTE_UPSERT',1,'AUDIT_ONLY','platform.tokens.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_ROUTE_DELETE',1,'AUDIT_ONLY','platform.tokens.manage','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_CREDENTIAL_VALIDATE',1,'AUDIT_ONLY','platform.tokens.credentials.validate','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_USAGE_RECONCILIATION_RETRY',1,'AUDIT_ONLY','platform.usage.reconcile.retry','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('SUPER_ADMIN_ROLE_HANDOFF',1,'AUDIT_ONLY','platform.roles.assign','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BREAK_GLASS_RECOVERY',1,'AUDIT_ONLY','platform.roles.assign','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('SUPER_ADMIN_BOOTSTRAP',1,'AUDIT_ONLY','platform.roles.assign','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('APPROVAL_DRAFT_CREATE',1,'AUDIT_ONLY','platform.billing.repairs.request','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('APPROVAL_DRAFT_UPDATE',1,'AUDIT_ONLY','platform.billing.repairs.request','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('APPROVAL_REQUEST_SUBMIT',1,'AUDIT_ONLY','platform.billing.repairs.request','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('APPROVAL_REQUEST_WITHDRAW',1,'AUDIT_ONLY','platform.billing.repairs.request','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('APPROVAL_REQUEST_DECIDE',1,'AUDIT_ONLY','platform.billing.repairs.approve','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('APPROVAL_EXECUTION_BEGIN',1,'AUDIT_ONLY','internal.approval.execute','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('APPROVAL_EXECUTION_COMPLETE',1,'AUDIT_ONLY','internal.approval.execute','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_WALLET_PROVISION',1,'AUDIT_ONLY','internal.account.wallet.provision','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_ROLE_BINDING_EXPIRE',1,'AUDIT_ONLY','internal.account.role-binding.expire','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_APPROVAL_EXPIRE',1,'AUDIT_ONLY','internal.account.approval.expire','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_PROFILE_CHANGE_RECOVERY',1,'AUDIT_ONLY','internal.account.profile-change.recover','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_REGISTRATION_RECOVERY',1,'AUDIT_ONLY','internal.account.registration.recover','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('ACCOUNT_COMMERCE_CUSTOMER_SYNC',1,'AUDIT_ONLY','internal.account.commerce-customer.sync','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_PAYMENT_EXPIRY_RECONCILE',1,'AUDIT_ONLY','internal.billing.payment.reconcile','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('BILLING_ENTITLEMENT_PACKAGE_EXPIRE',1,'AUDIT_ONLY','internal.billing.entitlement.expire','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('TOKEN_USAGE_RECONCILE',1,'AUDIT_ONLY','internal.token.usage.reconcile','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now()),
('EVENT_OUTBOX_RELAY',1,'AUDIT_ONLY','internal.events.relay','GOVERNANCE_OR_NORMAL_STATE_MACHINE',0,false,1,'["target_id","request_fingerprint"]',INTERVAL '24 hours',INTERVAL '5 minutes','requesting-domain-service',now());
