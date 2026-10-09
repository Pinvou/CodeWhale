ALTER TABLE approval_executions
    ADD COLUMN critical_resource_versions JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN financial_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE OR REPLACE FUNCTION protect_approval_execution() RETURNS trigger LANGUAGE plpgsql AS $$
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
		OR NEW.executor_identity IS DISTINCT FROM OLD.executor_identity
		OR NEW.critical_resource_versions IS DISTINCT FROM OLD.critical_resource_versions
		OR NEW.financial_snapshot IS DISTINCT FROM OLD.financial_snapshot THEN
		RAISE EXCEPTION 'approval execution identity is immutable' USING ERRCODE='55000';
	END IF;
	RETURN NEW;
END;
$$;
