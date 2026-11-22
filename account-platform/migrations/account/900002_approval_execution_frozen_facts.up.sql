-- Restore original 046 immutable execution facts after original 053 added authorization binding.
-- The original SQL versions stay unchanged; state/error/receipt retry updates remain legal.
CREATE OR REPLACE FUNCTION public.protect_approval_execution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
    (OLD.status='EXECUTING' AND NEW.status IN ('EXECUTED','FAILED')) OR (OLD.status='FAILED' AND NEW.status='EXECUTING')
  ) THEN RAISE EXCEPTION 'invalid approval execution state transition' USING ERRCODE='23514'; END IF;
  IF NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id OR NEW.command_type IS DISTINCT FROM OLD.command_type
     OR NEW.command_fingerprint IS DISTINCT FROM OLD.command_fingerprint OR NEW.execution_idempotency_key IS DISTINCT FROM OLD.execution_idempotency_key
     OR NEW.executor_identity IS DISTINCT FROM OLD.executor_identity OR NEW.authorization_event_id IS DISTINCT FROM OLD.authorization_event_id
     OR NEW.authorization_durable IS DISTINCT FROM OLD.authorization_durable
     OR NEW.authorization_approved_request_version IS DISTINCT FROM OLD.authorization_approved_request_version
     OR NEW.authorization_event_digest IS DISTINCT FROM OLD.authorization_event_digest
     OR NEW.authorization_digest_kind IS DISTINCT FROM OLD.authorization_digest_kind
     OR NEW.authorization_serialization_version IS DISTINCT FROM OLD.authorization_serialization_version
     OR NEW.authorization_provenance IS DISTINCT FROM OLD.authorization_provenance
     OR NEW.authorization_approval_epoch IS DISTINCT FROM OLD.authorization_approval_epoch
     OR NEW.critical_resource_versions IS DISTINCT FROM OLD.critical_resource_versions
     OR NEW.financial_snapshot IS DISTINCT FROM OLD.financial_snapshot THEN
    RAISE EXCEPTION 'approval execution identity is immutable' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END $$;
