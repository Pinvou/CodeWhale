DO $$ BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000053 down requires account_migrator with MIGRATION_ROLE account_service' USING ERRCODE='42501';
  END IF;
  IF EXISTS(SELECT 1 FROM public.approval_executions WHERE authorization_event_id IS NOT NULL) THEN
    RAISE EXCEPTION 'approval consumer history must be preserved' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DO $$ BEGIN
  IF EXISTS(SELECT 1 FROM account_approval_authorization.approval_delivery_states) THEN
    RAISE EXCEPTION 'approval consumer history must be preserved' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
DROP TRIGGER approval_executions_authorization_guard ON public.approval_executions;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DROP FUNCTION public.record_approval_authorization_terminal(UUID,TEXT,TEXT,BIGINT);
DROP FUNCTION public.record_approval_authorization_dlq_puback(UUID,TEXT,TEXT,BIGINT,TEXT,BIGINT);
DROP FUNCTION public.record_approval_authorization_result(UUID,TEXT,TEXT,BIGINT);
DROP FUNCTION public.record_approval_authorization_domain_failure(UUID,TEXT,TEXT,BIGINT,TEXT);
DROP FUNCTION public.grant_approval_authorization_domain_attempt(UUID,TEXT,TEXT,BIGINT);
DROP FUNCTION public.guard_approval_execution_authorization();
DROP FUNCTION public.validate_approval_authorization_begin(UUID,TEXT,TEXT,UUID,BIGINT,BIGINT,TEXT,TEXT,TEXT,TEXT,TEXT,BIGINT,TEXT);
DROP FUNCTION public.validate_legacy_approval_authorization_begin(UUID,TEXT);
DROP FUNCTION public.observe_approval_authorization_delivery(UUID,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,BIGINT,BIGINT,TEXT,BIGINT,TEXT,BIGINT,TEXT);
DROP FUNCTION public.resolve_approval_authorization_binding(UUID,TEXT,TEXT);
DROP FUNCTION account_approval_authorization.validate_legacy_authorization_begin(UUID,TEXT);
DROP FUNCTION account_approval_authorization.resolve_authorization_binding(UUID,TEXT,TEXT);
DROP TRIGGER approval_delivery_states_no_remove ON account_approval_authorization.approval_delivery_states;
DROP FUNCTION account_approval_authorization.reject_approval_delivery_state_removal();
DROP TRIGGER approval_delivery_states_guard ON account_approval_authorization.approval_delivery_states;
DROP FUNCTION account_approval_authorization.protect_approval_delivery_state();
DROP FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(TEXT,TEXT,TEXT,BIGINT,TEXT);
DROP TABLE account_approval_authorization.approval_delivery_states;

RESET ROLE;
SET ROLE account_service;
REVOKE SELECT ON public.approval_requests,public.approval_command_policies
  FROM account_approval_authorization_owner;
REVOKE UPDATE(id) ON public.approval_requests FROM account_approval_authorization_owner;
DROP INDEX public.approval_executions_authorization_event_uidx;
ALTER TABLE public.approval_executions DROP CONSTRAINT approval_execution_authorization_binding;
ALTER TABLE public.approval_executions
  DROP COLUMN authorization_event_id,
  DROP COLUMN authorization_durable,
  DROP COLUMN authorization_approved_request_version,
  DROP COLUMN authorization_event_digest,
  DROP COLUMN authorization_digest_kind,
  DROP COLUMN authorization_serialization_version,
  DROP COLUMN authorization_provenance,
  DROP COLUMN authorization_approval_epoch;

CREATE OR REPLACE FUNCTION public.protect_approval_execution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
    (OLD.status='EXECUTING' AND NEW.status IN ('EXECUTED','FAILED')) OR (OLD.status='FAILED' AND NEW.status='EXECUTING')
  ) THEN RAISE EXCEPTION 'invalid approval execution state transition' USING ERRCODE='23514'; END IF;
  IF NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id OR NEW.command_type IS DISTINCT FROM OLD.command_type
     OR NEW.command_fingerprint IS DISTINCT FROM OLD.command_fingerprint OR NEW.execution_idempotency_key IS DISTINCT FROM OLD.execution_idempotency_key
     OR NEW.executor_identity IS DISTINCT FROM OLD.executor_identity THEN
    RAISE EXCEPTION 'approval execution identity is immutable' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END $$;

DO $$ BEGIN
  IF EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
        WHERE n.nspname='account_approval_authorization' AND c.relname='approval_delivery_states')
     OR EXISTS(SELECT 1 FROM pg_catalog.pg_attribute
        WHERE attrelid='public.approval_executions'::regclass
          AND attname IN ('authorization_event_id','authorization_durable','authorization_approved_request_version',
            'authorization_event_digest','authorization_digest_kind','authorization_serialization_version',
            'authorization_provenance','authorization_approval_epoch') AND NOT attisdropped)
     OR pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE') THEN
    RAISE EXCEPTION '000053 down left protected objects or grants' USING ERRCODE='55000';
  END IF;
END $$;
