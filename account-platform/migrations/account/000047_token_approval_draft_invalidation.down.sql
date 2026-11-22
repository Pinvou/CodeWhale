CREATE OR REPLACE FUNCTION protect_submitted_approval_request() RETURNS trigger LANGUAGE plpgsql AS $$
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
