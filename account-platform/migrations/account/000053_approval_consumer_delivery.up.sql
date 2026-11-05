-- GSA-08: Account-owned authorization binding and persistent delivery state.
DO $$
DECLARE
  owner_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_authorization_owner');
  migrator_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_migrator');
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000053 requires account_migrator with MIGRATION_ROLE account_service' USING ERRCODE='42501';
  END IF;
  IF owner_oid IS NULL OR migrator_oid IS NULL OR NOT EXISTS(
      SELECT 1 FROM pg_catalog.pg_auth_members
       WHERE roleid=owner_oid AND member=migrator_oid AND admin_option AND set_option AND NOT inherit_option
  ) THEN
    RAISE EXCEPTION '000053 authorization owner topology is invalid' USING ERRCODE='55000';
  END IF;
  IF pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.outbox_events'::regclass))<>'account_approval_authorization_owner'
     OR pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.approval_executions'::regclass))<>'account_service'
     OR pg_catalog.pg_get_userbyid((SELECT nspowner FROM pg_catalog.pg_namespace WHERE nspname='account_approval_authorization'))<>'account_approval_authorization_owner'
     OR EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
         WHERE n.nspname='account_approval_authorization' AND c.relname='approval_delivery_states') THEN
    RAISE EXCEPTION '000053 protected object preflight failed' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
GRANT REFERENCES(event_id) ON public.outbox_events TO account_service;
RESET ROLE;
SET ROLE account_service;

ALTER TABLE public.approval_executions
  ADD COLUMN authorization_event_id UUID REFERENCES public.outbox_events(event_id),
  ADD COLUMN authorization_durable TEXT,
  ADD COLUMN authorization_approved_request_version BIGINT,
  ADD COLUMN authorization_event_digest TEXT,
  ADD COLUMN authorization_digest_kind TEXT,
  ADD COLUMN authorization_serialization_version TEXT,
  ADD COLUMN authorization_provenance TEXT,
  ADD COLUMN authorization_approval_epoch BIGINT,
  ADD CONSTRAINT approval_execution_authorization_binding CHECK (
    (authorization_event_id IS NULL AND authorization_durable IS NULL
      AND authorization_approved_request_version IS NULL AND authorization_event_digest IS NULL
      AND authorization_digest_kind IS NULL AND authorization_serialization_version IS NULL
      AND authorization_provenance IS NULL AND authorization_approval_epoch IS NULL)
    OR
    (authorization_event_id IS NOT NULL
      AND authorization_durable IS NOT NULL AND char_length(authorization_durable) BETWEEN 1 AND 120
      AND authorization_durable!~'[[:cntrl:]]'
      AND authorization_approved_request_version IS NOT NULL AND authorization_approved_request_version>0
      AND authorization_event_digest IS NOT NULL AND authorization_event_digest~'^[0-9a-f]{64}$'
      AND authorization_digest_kind IS NOT NULL AND authorization_digest_kind IN ('DELIVERY_BYTES','LEGACY_SEMANTIC')
      AND authorization_serialization_version IS NOT NULL
      AND authorization_serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')
      AND authorization_provenance IS NOT NULL
      AND authorization_provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY','RECONSTRUCTED_AFTER_PUBLISH')
      AND authorization_approval_epoch IS NOT NULL AND authorization_approval_epoch>0
      AND ((authorization_digest_kind='DELIVERY_BYTES'
              AND authorization_provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY'))
        OR (authorization_digest_kind='LEGACY_SEMANTIC'
              AND authorization_serialization_version='LEGACY_TYPED_V1'
              AND authorization_provenance='RECONSTRUCTED_AFTER_PUBLISH')))
  );
CREATE UNIQUE INDEX approval_executions_authorization_event_uidx
  ON public.approval_executions(authorization_event_id) WHERE authorization_event_id IS NOT NULL;

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
     OR NEW.authorization_approval_epoch IS DISTINCT FROM OLD.authorization_approval_epoch THEN
    RAISE EXCEPTION 'approval execution identity is immutable' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END $$;

GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
GRANT SELECT ON public.approval_requests,public.approval_command_policies
  TO account_approval_authorization_owner;
GRANT UPDATE(id) ON public.approval_requests TO account_approval_authorization_owner;
-- Required only while the owner creates the delivery-state FK. It is revoked
-- before the migration returns to the runtime role.
GRANT REFERENCES(id) ON public.approval_requests TO account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;

DO $$
DECLARE summary RECORD;
BEGIN
  SELECT * INTO STRICT summary FROM account_approval_authorization.authorization_sidecar_summary();
  IF summary.missing_epoch_count<>0 OR summary.missing_sidecar_count<>0 OR summary.invalid_sidecar_count<>0 THEN
    RAISE EXCEPTION '000053 requires a complete valid authorization sidecar history' USING ERRCODE='55000';
  END IF;
END $$;

CREATE TABLE account_approval_authorization.approval_delivery_states (
  schema_version SMALLINT NOT NULL CHECK(schema_version=1),
  original_event_id UUID NOT NULL REFERENCES public.outbox_events(event_id) ON DELETE RESTRICT,
  durable TEXT NOT NULL CHECK(char_length(durable) BETWEEN 1 AND 120 AND durable!~'[[:cntrl:]]'),
  approval_request_id UUID NOT NULL REFERENCES public.approval_requests(id) ON DELETE RESTRICT,
  approved_request_version BIGINT NOT NULL CHECK(approved_request_version>0),
  event_digest TEXT NOT NULL CHECK(event_digest~'^[0-9a-f]{64}$'),
  digest_kind TEXT NOT NULL CHECK(digest_kind IN ('DELIVERY_BYTES','LEGACY_SEMANTIC')),
  serialization_version TEXT NOT NULL CHECK(serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')),
  provenance TEXT NOT NULL CHECK(provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY','RECONSTRUCTED_AFTER_PUBLISH')),
  approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
  stream TEXT NOT NULL CHECK(stream='ACCOUNT_PLATFORM'),
  stream_sequence BIGINT NOT NULL CHECK(stream_sequence>0),
  command_type TEXT NOT NULL,
  policy_version BIGINT NOT NULL CHECK(policy_version>0),
  executor_identity TEXT NOT NULL CHECK(executor_identity IN ('billing-command-executor','token-command-executor')),
  domain_attempt_count SMALLINT NOT NULL DEFAULT 0 CHECK(domain_attempt_count BETWEEN 0 AND 12),
  state TEXT NOT NULL CHECK(state IN ('READY','DOMAIN_INFLIGHT','RESULT_CONFIRMED','DLQ_PUBLISHED_ACK_PENDING','TERMINAL_CONFIRMED')),
  dlq_message_id TEXT,
  dlq_stream_sequence BIGINT,
  last_error_code TEXT CHECK(last_error_code IS NULL OR (last_error_code~'^[A-Z][A-Z0-9_]{0,119}$'
    AND last_error_code!~'(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)')),
  observed_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
  version BIGINT NOT NULL DEFAULT 1 CHECK(version>0),
  PRIMARY KEY(original_event_id,durable),
  CONSTRAINT approval_delivery_digest_provenance CHECK (
    (digest_kind='DELIVERY_BYTES' AND provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY'))
    OR (digest_kind='LEGACY_SEMANTIC' AND serialization_version='LEGACY_TYPED_V1' AND provenance='RECONSTRUCTED_AFTER_PUBLISH')
  ),
  CONSTRAINT approval_delivery_attempt_shape CHECK (
    (executor_identity='token-command-executor' AND domain_attempt_count=0 AND state<>'DOMAIN_INFLIGHT'
      AND dlq_message_id IS NULL AND dlq_stream_sequence IS NULL AND last_error_code IS NULL)
    OR executor_identity='billing-command-executor'
  ),
  CONSTRAINT approval_delivery_dlq_shape CHECK (
    (state='DLQ_PUBLISHED_ACK_PENDING' AND executor_identity='billing-command-executor' AND domain_attempt_count=12
      AND dlq_message_id IS NOT NULL AND char_length(dlq_message_id) BETWEEN 1 AND 240
      AND dlq_stream_sequence IS NOT NULL AND dlq_stream_sequence>0 AND last_error_code IS NOT NULL)
    OR (state<>'DLQ_PUBLISHED_ACK_PENDING' AND dlq_message_id IS NULL AND dlq_stream_sequence IS NULL)
    OR (state='TERMINAL_CONFIRMED' AND executor_identity='billing-command-executor' AND domain_attempt_count=12
      AND dlq_message_id IS NOT NULL AND dlq_stream_sequence IS NOT NULL AND last_error_code IS NOT NULL)
  )
);
REVOKE ALL ON account_approval_authorization.approval_delivery_states
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(
  p_durable TEXT,p_workload TEXT,p_command_type TEXT,p_policy_version BIGINT,p_executor_identity TEXT
) RETURNS BOOLEAN LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
SELECT COALESCE(
 (p_durable='billing-wallet-adjustment-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='WALLET_ADJUSTMENT' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-payment-provider-resume-executor-p2-v1' AND p_workload='billing-command-executor'
    AND p_command_type='PAYMENT_PROVIDER_RESUME' AND p_policy_version=2 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-payment-product-resume-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='PAYMENT_PRODUCT_RESUME' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
 OR (p_durable='token-model-lifecycle-executor-v1' AND p_workload='token-command-executor'
    AND p_command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND p_policy_version=2 AND p_executor_identity='token-command-executor')
 OR (p_durable='token-catalog-rollback-executor-v1' AND p_workload='token-command-executor'
    AND p_command_type='TOKEN_MODEL_CATALOG_ROLLBACK' AND p_policy_version=2 AND p_executor_identity='token-command-executor')
 OR (p_durable='token-credential-revoke-executor-v1' AND p_workload='token-command-executor'
    AND p_command_type='TOKEN_CREDENTIAL_REVOKE' AND p_policy_version=1 AND p_executor_identity='token-command-executor')
 OR (p_durable='billing-command-executor-v1' AND p_workload='billing-command-executor'
    AND p_command_type='WALLET_ADJUSTMENT' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-command-executor-v1' AND p_workload='billing-command-executor'
    AND p_command_type='PAYMENT_PROVIDER_RESUME' AND p_policy_version=2 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-command-executor-v1' AND p_workload='billing-command-executor'
    AND p_command_type='PAYMENT_PRODUCT_RESUME' AND p_policy_version=1 AND p_executor_identity='billing-command-executor'),false)
$$;
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(TEXT,TEXT,TEXT,BIGINT,TEXT)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION account_approval_authorization.protect_approval_delivery_state() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.original_event_id IS DISTINCT FROM OLD.original_event_id
     OR NEW.durable IS DISTINCT FROM OLD.durable OR NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id
     OR NEW.approved_request_version IS DISTINCT FROM OLD.approved_request_version OR NEW.event_digest IS DISTINCT FROM OLD.event_digest
     OR NEW.digest_kind IS DISTINCT FROM OLD.digest_kind OR NEW.serialization_version IS DISTINCT FROM OLD.serialization_version
     OR NEW.provenance IS DISTINCT FROM OLD.provenance OR NEW.approval_epoch IS DISTINCT FROM OLD.approval_epoch
     OR NEW.stream IS DISTINCT FROM OLD.stream OR NEW.stream_sequence IS DISTINCT FROM OLD.stream_sequence
     OR NEW.command_type IS DISTINCT FROM OLD.command_type OR NEW.policy_version IS DISTINCT FROM OLD.policy_version
     OR NEW.executor_identity IS DISTINCT FROM OLD.executor_identity OR NEW.observed_at IS DISTINCT FROM OLD.observed_at
     OR NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at OR NEW.domain_attempt_count<OLD.domain_attempt_count
     OR NEW.domain_attempt_count>OLD.domain_attempt_count+1 THEN
    RAISE EXCEPTION 'approval delivery identity or version is immutable' USING ERRCODE='55000';
  END IF;
  IF NOT ((OLD.state='READY' AND NEW.state IN ('READY','DOMAIN_INFLIGHT','RESULT_CONFIRMED','TERMINAL_CONFIRMED'))
    OR (OLD.state='DOMAIN_INFLIGHT' AND NEW.state IN ('READY','DOMAIN_INFLIGHT','RESULT_CONFIRMED','DLQ_PUBLISHED_ACK_PENDING'))
    OR (OLD.state='RESULT_CONFIRMED' AND NEW.state IN ('RESULT_CONFIRMED','TERMINAL_CONFIRMED'))
    OR (OLD.state='DLQ_PUBLISHED_ACK_PENDING' AND NEW.state IN ('DLQ_PUBLISHED_ACK_PENDING','TERMINAL_CONFIRMED'))
    OR (OLD.state='TERMINAL_CONFIRMED' AND NEW.state='TERMINAL_CONFIRMED')) THEN
    RAISE EXCEPTION 'invalid approval delivery state transition' USING ERRCODE='23514';
  END IF;
  IF NEW.domain_attempt_count=OLD.domain_attempt_count+1 AND NOT (OLD.state='READY' AND NEW.state='DOMAIN_INFLIGHT') THEN
    RAISE EXCEPTION 'domain attempt may increment only when granted' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER approval_delivery_states_guard BEFORE UPDATE
  ON account_approval_authorization.approval_delivery_states FOR EACH ROW
  EXECUTE FUNCTION account_approval_authorization.protect_approval_delivery_state();
CREATE FUNCTION account_approval_authorization.reject_approval_delivery_state_removal() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN RAISE EXCEPTION 'approval delivery state is append-only' USING ERRCODE='55000'; END $$;
CREATE TRIGGER approval_delivery_states_no_remove BEFORE DELETE OR TRUNCATE
  ON account_approval_authorization.approval_delivery_states FOR EACH STATEMENT
  EXECUTE FUNCTION account_approval_authorization.reject_approval_delivery_state_removal();
REVOKE ALL ON FUNCTION account_approval_authorization.protect_approval_delivery_state()
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
REVOKE ALL ON FUNCTION account_approval_authorization.reject_approval_delivery_state_removal()
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION account_approval_authorization.resolve_authorization_binding(
  p_event_id UUID,p_durable TEXT,p_workload TEXT
) RETURNS TABLE(approval_request_id UUID,approved_request_version BIGINT,approval_epoch BIGINT,
  serialization_version TEXT,provenance TEXT,event_digest TEXT,digest_kind TEXT,
  command_type TEXT,policy_version BIGINT,executor_identity TEXT)
LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
  SELECT o.aggregate_id,o.aggregate_version,o.approval_control_epoch,s.serialization_version,s.provenance,
      CASE WHEN s.provenance='RECONSTRUCTED_AFTER_PUBLISH' THEN s.reconstructed_semantic_digest ELSE s.event_digest END,
      CASE WHEN s.provenance='RECONSTRUCTED_AFTER_PUBLISH' THEN 'LEGACY_SEMANTIC' ELSE 'DELIVERY_BYTES' END,
      r.command_type,r.policy_version,p.executor_identity
    FROM public.outbox_events o
    JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
    JOIN public.approval_requests r ON r.id=o.aggregate_id
    JOIN public.approval_command_policies p ON p.command_type=r.command_type AND p.policy_version=r.policy_version
   WHERE o.event_id=p_event_id AND o.event_type='account.approval-command.authorized.v1'
     AND o.published_at IS NOT NULL AND o.dead_lettered_at IS NULL
     AND o.aggregate_version>0 AND o.approval_control_epoch>0
     AND NOT EXISTS(SELECT 1 FROM account_approval_authorization.authorization_event_pending x WHERE x.event_id=o.event_id)
     AND NOT EXISTS(SELECT 1 FROM account_approval_authorization.authorization_delivery_pending x WHERE x.event_id=o.event_id)
     AND account_approval_authorization.authorization_consumer_mapping_allows(p_durable,p_workload,r.command_type,r.policy_version,p.executor_identity)
     AND ((s.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY') AND s.delivery_bytes IS NOT NULL
           AND s.event_digest IS NOT NULL AND s.event_digest=pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex')
           AND account_approval_authorization.authorization_delivery_matches(o.event_id,o.event_type,o.aggregate_id,o.aggregate_version,
               o.payload,o.trace_id,o.occurred_at,o.approval_control_epoch,s.serialization_version,s.delivery_bytes))
       OR (s.provenance='RECONSTRUCTED_AFTER_PUBLISH' AND s.serialization_version='LEGACY_TYPED_V1'
           AND s.delivery_bytes IS NULL AND s.event_digest IS NULL
           AND s.reconstructed_semantic_digest IS NOT NULL AND s.reconstructed_semantic_digest~'^[0-9a-f]{64}$'))
$$;
REVOKE ALL ON FUNCTION account_approval_authorization.resolve_authorization_binding(UUID,TEXT,TEXT)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION public.resolve_approval_authorization_binding(p_event_id UUID,p_durable TEXT,p_workload TEXT)
RETURNS TABLE(approval_request_id UUID,approved_request_version BIGINT,approval_epoch BIGINT,
  serialization_version TEXT,provenance TEXT,event_digest TEXT,digest_kind TEXT,
  command_type TEXT,policy_version BIGINT,executor_identity TEXT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_event_id IS NULL OR p_durable IS NULL OR p_workload IS NULL
     OR p_durable<>btrim(p_durable) OR p_workload<>btrim(p_workload)
     OR p_durable~'[[:cntrl:]]' OR p_workload~'[[:cntrl:]]' THEN
    RAISE EXCEPTION 'authorization binding reader forbidden' USING ERRCODE='42501';
  END IF;
  RETURN QUERY SELECT * FROM account_approval_authorization.resolve_authorization_binding(p_event_id,p_durable,p_workload);
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization binding is unavailable' USING ERRCODE='55000'; END IF;
END $$;

CREATE FUNCTION public.observe_approval_authorization_delivery(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_digest TEXT,p_digest_kind TEXT,p_serialization TEXT,p_provenance TEXT,
  p_approved_request_version BIGINT,p_approval_epoch BIGINT,p_stream TEXT,p_stream_sequence BIGINT,
  p_command_type TEXT,p_policy_version BIGINT,p_executor TEXT
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE binding RECORD;
BEGIN
  IF session_user<>'account_service' OR p_stream<>'ACCOUNT_PLATFORM' OR p_stream_sequence<=0 THEN
    RAISE EXCEPTION 'authorization delivery observation forbidden' USING ERRCODE='42501';
  END IF;
  SELECT * INTO STRICT binding FROM account_approval_authorization.resolve_authorization_binding(p_event_id,p_durable,p_workload);
  IF binding.approved_request_version IS DISTINCT FROM p_approved_request_version OR binding.approval_epoch IS DISTINCT FROM p_approval_epoch
     OR binding.serialization_version IS DISTINCT FROM p_serialization OR binding.provenance IS DISTINCT FROM p_provenance
     OR binding.event_digest IS DISTINCT FROM p_digest OR binding.digest_kind IS DISTINCT FROM p_digest_kind
     OR binding.command_type IS DISTINCT FROM p_command_type OR binding.policy_version IS DISTINCT FROM p_policy_version
     OR binding.executor_identity IS DISTINCT FROM p_executor THEN
    RAISE EXCEPTION 'authorization delivery binding mismatch' USING ERRCODE='55000';
  END IF;
  INSERT INTO account_approval_authorization.approval_delivery_states(
    schema_version,original_event_id,durable,approval_request_id,approved_request_version,event_digest,digest_kind,
    serialization_version,provenance,approval_epoch,stream,stream_sequence,command_type,policy_version,executor_identity,state)
  VALUES(1,p_event_id,p_durable,binding.approval_request_id,p_approved_request_version,p_digest,p_digest_kind,
    p_serialization,p_provenance,p_approval_epoch,p_stream,p_stream_sequence,p_command_type,p_policy_version,p_executor,'READY')
  ON CONFLICT(original_event_id,durable) DO NOTHING;
  RETURN QUERY SELECT d.domain_attempt_count,d.state,d.version
    FROM account_approval_authorization.approval_delivery_states d
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.approval_request_id=binding.approval_request_id
     AND d.approved_request_version=p_approved_request_version AND d.event_digest=p_digest AND d.digest_kind=p_digest_kind
     AND d.serialization_version=p_serialization AND d.provenance=p_provenance AND d.approval_epoch=p_approval_epoch
     AND d.stream=p_stream AND d.command_type=p_command_type AND d.policy_version=p_policy_version AND d.executor_identity=p_executor;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization delivery observation conflict' USING ERRCODE='55000'; END IF;
END $$;

CREATE FUNCTION public.validate_approval_authorization_begin(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_request_id UUID,p_approved_request_version BIGINT,p_approval_epoch BIGINT,
  p_digest TEXT,p_digest_kind TEXT,p_serialization TEXT,p_provenance TEXT,p_command_type TEXT,p_policy_version BIGINT,p_executor TEXT
) RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row RECORD; request_row RECORD; outbox_row RECORD; binding RECORD; delivery_row RECORD;
BEGIN
  IF session_user<>'account_service' THEN RAISE EXCEPTION 'authorization begin forbidden' USING ERRCODE='42501'; END IF;
  SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
  SELECT id,status,version,command_type,policy_version INTO STRICT request_row
    FROM public.approval_requests WHERE id=p_request_id FOR UPDATE;
  SELECT event_id,aggregate_id,aggregate_version,approval_control_epoch,published_at,dead_lettered_at INTO STRICT outbox_row
    FROM public.outbox_events WHERE event_id=p_event_id FOR SHARE;
  SELECT * INTO STRICT binding FROM account_approval_authorization.resolve_authorization_binding(p_event_id,p_durable,p_workload);
  SELECT * INTO STRICT delivery_row FROM account_approval_authorization.approval_delivery_states
    WHERE original_event_id=p_event_id AND durable=p_durable FOR SHARE;
  IF control_row.schema_version<>1 OR p_request_id IS DISTINCT FROM binding.approval_request_id
     OR p_approved_request_version IS DISTINCT FROM binding.approved_request_version OR p_approval_epoch IS DISTINCT FROM binding.approval_epoch
     OR p_digest IS DISTINCT FROM binding.event_digest OR p_digest_kind IS DISTINCT FROM binding.digest_kind
     OR p_serialization IS DISTINCT FROM binding.serialization_version OR p_provenance IS DISTINCT FROM binding.provenance
     OR p_command_type IS DISTINCT FROM binding.command_type OR p_policy_version IS DISTINCT FROM binding.policy_version
     OR p_executor IS DISTINCT FROM binding.executor_identity OR p_executor IS DISTINCT FROM p_workload
     OR request_row.command_type IS DISTINCT FROM p_command_type OR request_row.policy_version IS DISTINCT FROM p_policy_version
     OR request_row.status NOT IN ('APPROVED','EXECUTION_FAILED','EXECUTING','EXECUTED')
     OR outbox_row.aggregate_id IS DISTINCT FROM p_request_id OR outbox_row.aggregate_version IS DISTINCT FROM p_approved_request_version
     OR outbox_row.approval_control_epoch IS DISTINCT FROM p_approval_epoch OR outbox_row.published_at IS NULL OR outbox_row.dead_lettered_at IS NOT NULL
     OR delivery_row.approval_request_id IS DISTINCT FROM p_request_id
     OR delivery_row.approved_request_version IS DISTINCT FROM p_approved_request_version
     OR delivery_row.event_digest IS DISTINCT FROM p_digest OR delivery_row.digest_kind IS DISTINCT FROM p_digest_kind
     OR delivery_row.serialization_version IS DISTINCT FROM p_serialization OR delivery_row.provenance IS DISTINCT FROM p_provenance
     OR delivery_row.approval_epoch IS DISTINCT FROM p_approval_epoch OR delivery_row.command_type IS DISTINCT FROM p_command_type
     OR delivery_row.policy_version IS DISTINCT FROM p_policy_version OR delivery_row.executor_identity IS DISTINCT FROM p_executor
     OR (p_workload='billing-command-executor' AND delivery_row.state<>'DOMAIN_INFLIGHT')
     OR (p_workload='token-command-executor' AND delivery_row.state<>'READY')
     OR NOT account_approval_authorization.authorization_consumer_mapping_allows(p_durable,p_workload,p_command_type,p_policy_version,p_executor) THEN
    RAISE EXCEPTION 'authorization begin binding mismatch' USING ERRCODE='55000';
  END IF;
END $$;

-- Old binaries cannot carry the event binding. They remain usable only for
-- the first LEGACY -> QUIESCING drain and only for immutable legacy epoch-1
-- authorization events. Returning to LEGACY at a later epoch cannot revive
-- this compatibility path.
CREATE FUNCTION account_approval_authorization.validate_legacy_authorization_begin(
  p_request_id UUID,p_executor TEXT
) RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE control_row RECORD; request_row RECORD; outbox_row RECORD; sidecar_row RECORD; legacy_durable TEXT;
BEGIN
  IF session_user<>'account_service' OR p_request_id IS NULL
     OR p_executor NOT IN ('billing-command-executor','token-command-executor') THEN
    RAISE EXCEPTION 'legacy authorization begin forbidden' USING ERRCODE='42501';
  END IF;
  SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
  IF NOT ((control_row.mode='LEGACY' AND control_row.approval_epoch=1)
       OR (control_row.mode='QUIESCING' AND control_row.approval_epoch=2))
     OR control_row.approval_wire_version<>1 OR control_row.proof_required THEN
    RAISE EXCEPTION 'legacy authorization begin is no longer available' USING ERRCODE='55000';
  END IF;
  SELECT id,status,command_type,policy_version INTO STRICT request_row
    FROM public.approval_requests WHERE id=p_request_id FOR UPDATE;
  SELECT o.event_id,o.aggregate_version,o.approval_control_epoch,o.payload,o.trace_id,o.occurred_at,
         s.serialization_version,s.provenance,s.delivery_bytes,s.event_digest,s.reconstructed_semantic_digest
    INTO STRICT outbox_row
    FROM public.outbox_events o
    JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
   WHERE o.aggregate_id=p_request_id AND o.event_type='account.approval-command.authorized.v1'
     AND o.approval_control_epoch=1 AND o.published_at IS NOT NULL AND o.dead_lettered_at IS NULL
     AND NOT EXISTS(SELECT 1 FROM account_approval_authorization.authorization_event_pending x WHERE x.event_id=o.event_id)
     AND NOT EXISTS(SELECT 1 FROM account_approval_authorization.authorization_delivery_pending x WHERE x.event_id=o.event_id)
   FOR SHARE OF o;
  legacy_durable := CASE request_row.command_type
    WHEN 'WALLET_ADJUSTMENT' THEN CASE WHEN p_executor='billing-command-executor' THEN 'billing-command-executor-v1' END
    WHEN 'PAYMENT_PROVIDER_RESUME' THEN CASE WHEN p_executor='billing-command-executor' THEN 'billing-command-executor-v1' END
    WHEN 'PAYMENT_PRODUCT_RESUME' THEN CASE WHEN p_executor='billing-command-executor' THEN 'billing-command-executor-v1' END
    WHEN 'TOKEN_MODEL_LIFECYCLE_CHANGE' THEN CASE WHEN p_executor='token-command-executor' THEN 'token-model-lifecycle-executor-v1' END
    WHEN 'TOKEN_MODEL_CATALOG_ROLLBACK' THEN CASE WHEN p_executor='token-command-executor' THEN 'token-catalog-rollback-executor-v1' END
    WHEN 'TOKEN_CREDENTIAL_REVOKE' THEN CASE WHEN p_executor='token-command-executor' THEN 'token-credential-revoke-executor-v1' END
  END;
  IF request_row.status NOT IN ('APPROVED','EXECUTION_FAILED','EXECUTING','EXECUTED')
     OR outbox_row.serialization_version<>'LEGACY_TYPED_V1' OR legacy_durable IS NULL
     OR NOT account_approval_authorization.authorization_consumer_mapping_allows(
       legacy_durable,p_executor,request_row.command_type,request_row.policy_version,p_executor)
     OR NOT COALESCE(((outbox_row.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY')
          AND outbox_row.delivery_bytes IS NOT NULL AND outbox_row.event_digest IS NOT NULL
          AND outbox_row.event_digest=pg_catalog.encode(pg_catalog.sha256(outbox_row.delivery_bytes),'hex')
          AND account_approval_authorization.authorization_delivery_matches(
            outbox_row.event_id,'account.approval-command.authorized.v1',p_request_id,outbox_row.aggregate_version,
            outbox_row.payload,outbox_row.trace_id,outbox_row.occurred_at,1,
            outbox_row.serialization_version,outbox_row.delivery_bytes))
       OR (outbox_row.provenance='RECONSTRUCTED_AFTER_PUBLISH' AND outbox_row.delivery_bytes IS NULL
          AND outbox_row.event_digest IS NULL AND outbox_row.reconstructed_semantic_digest~'^[0-9a-f]{64}$')),false) THEN
    RAISE EXCEPTION 'legacy authorization begin binding mismatch' USING ERRCODE='55000';
  END IF;
END $$;

CREATE FUNCTION public.validate_legacy_approval_authorization_begin(p_request_id UUID,p_executor TEXT)
RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' THEN
    RAISE EXCEPTION 'legacy authorization begin forbidden' USING ERRCODE='42501';
  END IF;
  PERFORM account_approval_authorization.validate_legacy_authorization_begin(p_request_id,p_executor);
END $$;

CREATE FUNCTION public.guard_approval_execution_authorization() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE request_policy_version BIGINT;
BEGIN
  IF session_user<>'account_service' THEN
    RAISE EXCEPTION 'approval execution authorization guard forbidden' USING ERRCODE='42501';
  END IF;
  IF NEW.authorization_event_id IS NULL THEN
    PERFORM account_approval_authorization.validate_legacy_authorization_begin(
      NEW.approval_request_id,NEW.executor_identity);
  ELSIF TG_OP='INSERT' THEN
    SELECT policy_version INTO STRICT request_policy_version
      FROM public.approval_requests WHERE id=NEW.approval_request_id;
    PERFORM public.validate_approval_authorization_begin(
      NEW.authorization_event_id,NEW.authorization_durable,NEW.executor_identity,NEW.approval_request_id,
      NEW.authorization_approved_request_version,NEW.authorization_approval_epoch,NEW.authorization_event_digest,
      NEW.authorization_digest_kind,NEW.authorization_serialization_version,NEW.authorization_provenance,
      NEW.command_type,request_policy_version,NEW.executor_identity);
  ELSIF OLD.authorization_event_id IS NULL THEN
    SELECT policy_version INTO STRICT request_policy_version
      FROM public.approval_requests WHERE id=NEW.approval_request_id;
    PERFORM public.validate_approval_authorization_begin(
      NEW.authorization_event_id,NEW.authorization_durable,NEW.executor_identity,NEW.approval_request_id,
      NEW.authorization_approved_request_version,NEW.authorization_approval_epoch,NEW.authorization_event_digest,
      NEW.authorization_digest_kind,NEW.authorization_serialization_version,NEW.authorization_provenance,
      NEW.command_type,request_policy_version,NEW.executor_identity);
  END IF;
  RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION account_approval_authorization.validate_legacy_authorization_begin(UUID,TEXT)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
REVOKE ALL ON FUNCTION public.validate_legacy_approval_authorization_begin(UUID,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.guard_approval_execution_authorization()
  FROM PUBLIC,account_approval_writer,account_approval_backfill,account_approval_relay;
GRANT EXECUTE ON FUNCTION public.guard_approval_execution_authorization() TO account_service;

CREATE FUNCTION public.grant_approval_authorization_domain_attempt(p_event_id UUID,p_durable TEXT,p_workload TEXT,p_expected_version BIGINT)
RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_workload<>'billing-command-executor' THEN
    RAISE EXCEPTION 'authorization domain attempt forbidden' USING ERRCODE='42501';
  END IF;
  RETURN QUERY UPDATE account_approval_authorization.approval_delivery_states d
    SET domain_attempt_count=d.domain_attempt_count+1,state='DOMAIN_INFLIGHT',last_error_code=NULL,
        updated_at=transaction_timestamp(),version=d.version+1
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.version=p_expected_version AND d.state='READY'
     AND d.domain_attempt_count<12 AND d.executor_identity=p_workload
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization domain attempt CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

CREATE FUNCTION public.record_approval_authorization_domain_failure(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_expected_version BIGINT,p_error_code TEXT
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_workload<>'billing-command-executor' OR p_error_code IS NULL
     OR p_error_code!~'^[A-Z][A-Z0-9_]{0,119}$' OR p_error_code~'(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)' THEN
    RAISE EXCEPTION 'authorization domain failure invalid' USING ERRCODE='42501';
  END IF;
  RETURN QUERY UPDATE account_approval_authorization.approval_delivery_states d
    SET state=CASE WHEN d.domain_attempt_count<12 THEN 'READY' ELSE 'DOMAIN_INFLIGHT' END,last_error_code=p_error_code,
        updated_at=transaction_timestamp(),version=d.version+1
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.version=p_expected_version
     AND d.state='DOMAIN_INFLIGHT' AND d.domain_attempt_count>0 AND d.executor_identity=p_workload
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization domain failure CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

CREATE FUNCTION public.record_approval_authorization_result(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_expected_version BIGINT
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_workload NOT IN ('billing-command-executor','token-command-executor') THEN
    RAISE EXCEPTION 'authorization result forbidden' USING ERRCODE='42501';
  END IF;
  RETURN QUERY UPDATE account_approval_authorization.approval_delivery_states d
    SET state='RESULT_CONFIRMED',last_error_code=NULL,updated_at=transaction_timestamp(),version=d.version+1
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.version=p_expected_version
     AND d.executor_identity=p_workload
     AND ((p_workload='billing-command-executor' AND d.state='DOMAIN_INFLIGHT')
       OR (p_workload='token-command-executor' AND d.state='READY'))
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization result CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

CREATE FUNCTION public.record_approval_authorization_dlq_puback(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_expected_version BIGINT,p_message_id TEXT,p_stream_sequence BIGINT
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_workload<>'billing-command-executor' OR p_stream_sequence<=0
     OR p_message_id IS DISTINCT FROM p_event_id::TEXT||':'||p_durable||':dead-letter' THEN
    RAISE EXCEPTION 'authorization DLQ PubAck invalid' USING ERRCODE='42501';
  END IF;
  RETURN QUERY UPDATE account_approval_authorization.approval_delivery_states d
    SET state='DLQ_PUBLISHED_ACK_PENDING',dlq_message_id=p_message_id,dlq_stream_sequence=p_stream_sequence,
        updated_at=transaction_timestamp(),version=d.version+1
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.version=p_expected_version
     AND d.state='DOMAIN_INFLIGHT' AND d.domain_attempt_count=12 AND d.last_error_code IS NOT NULL
     AND d.executor_identity=p_workload
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization DLQ PubAck CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

CREATE FUNCTION public.record_approval_authorization_terminal(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_expected_version BIGINT
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_workload NOT IN ('billing-command-executor','token-command-executor') THEN
    RAISE EXCEPTION 'authorization terminal confirmation forbidden' USING ERRCODE='42501';
  END IF;
  RETURN QUERY UPDATE account_approval_authorization.approval_delivery_states d
    SET state='TERMINAL_CONFIRMED',updated_at=transaction_timestamp(),version=d.version+1
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.version=p_expected_version
     AND d.executor_identity=p_workload
     AND ((p_workload='billing-command-executor' AND d.state IN ('RESULT_CONFIRMED','DLQ_PUBLISHED_ACK_PENDING'))
       OR (p_workload='token-command-executor' AND d.state='RESULT_CONFIRMED'))
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization terminal CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

REVOKE ALL ON FUNCTION public.resolve_approval_authorization_binding(UUID,TEXT,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.observe_approval_authorization_delivery(UUID,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,BIGINT,BIGINT,TEXT,BIGINT,TEXT,BIGINT,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.validate_approval_authorization_begin(UUID,TEXT,TEXT,UUID,BIGINT,BIGINT,TEXT,TEXT,TEXT,TEXT,TEXT,BIGINT,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.validate_legacy_approval_authorization_begin(UUID,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.grant_approval_authorization_domain_attempt(UUID,TEXT,TEXT,BIGINT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.record_approval_authorization_domain_failure(UUID,TEXT,TEXT,BIGINT,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.record_approval_authorization_result(UUID,TEXT,TEXT,BIGINT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.record_approval_authorization_dlq_puback(UUID,TEXT,TEXT,BIGINT,TEXT,BIGINT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.record_approval_authorization_terminal(UUID,TEXT,TEXT,BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.resolve_approval_authorization_binding(UUID,TEXT,TEXT) TO account_service;
GRANT EXECUTE ON FUNCTION public.observe_approval_authorization_delivery(UUID,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,BIGINT,BIGINT,TEXT,BIGINT,TEXT,BIGINT,TEXT) TO account_service;
GRANT EXECUTE ON FUNCTION public.validate_approval_authorization_begin(UUID,TEXT,TEXT,UUID,BIGINT,BIGINT,TEXT,TEXT,TEXT,TEXT,TEXT,BIGINT,TEXT) TO account_service;
GRANT EXECUTE ON FUNCTION public.validate_legacy_approval_authorization_begin(UUID,TEXT) TO account_service;
GRANT EXECUTE ON FUNCTION public.grant_approval_authorization_domain_attempt(UUID,TEXT,TEXT,BIGINT) TO account_service;
GRANT EXECUTE ON FUNCTION public.record_approval_authorization_domain_failure(UUID,TEXT,TEXT,BIGINT,TEXT) TO account_service;
GRANT EXECUTE ON FUNCTION public.record_approval_authorization_result(UUID,TEXT,TEXT,BIGINT) TO account_service;
GRANT EXECUTE ON FUNCTION public.record_approval_authorization_dlq_puback(UUID,TEXT,TEXT,BIGINT,TEXT,BIGINT) TO account_service;
GRANT EXECUTE ON FUNCTION public.record_approval_authorization_terminal(UUID,TEXT,TEXT,BIGINT) TO account_service;

RESET ROLE;
SET ROLE account_service;
CREATE TRIGGER approval_executions_authorization_guard BEFORE INSERT OR UPDATE
  ON public.approval_executions FOR EACH ROW EXECUTE FUNCTION public.guard_approval_execution_authorization();
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
REVOKE REFERENCES(id) ON public.approval_requests FROM account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
REVOKE EXECUTE ON FUNCTION public.guard_approval_execution_authorization() FROM account_service;
REVOKE REFERENCES(event_id) ON public.outbox_events FROM account_service;
DO $$
DECLARE unexpected BIGINT;
BEGIN
  IF pg_catalog.has_table_privilege('account_service','account_approval_authorization.approval_delivery_states'::regclass,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER')
     OR pg_catalog.has_any_column_privilege('account_service','account_approval_authorization.approval_delivery_states'::regclass,'SELECT,INSERT,UPDATE,REFERENCES')
     OR pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE') THEN
    RAISE EXCEPTION '000053 final relation ACL is invalid' USING ERRCODE='55000';
  END IF;
  SELECT count(*) INTO unexpected
    FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
   WHERE n.nspname='public'
     AND p.proname IN ('resolve_approval_authorization_binding','observe_approval_authorization_delivery',
       'validate_approval_authorization_begin','grant_approval_authorization_domain_attempt',
       'validate_legacy_approval_authorization_begin',
       'record_approval_authorization_domain_failure','record_approval_authorization_result',
       'record_approval_authorization_dlq_puback','record_approval_authorization_terminal')
     AND (pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner' OR NOT p.prosecdef
       OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
       OR NOT pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')
       OR pg_catalog.has_function_privilege('public',p.oid,'EXECUTE'));
  IF unexpected<>0 THEN RAISE EXCEPTION '000053 final function metadata is invalid' USING ERRCODE='55000'; END IF;
  IF pg_catalog.has_function_privilege('account_service','public.guard_approval_execution_authorization()'::regprocedure,'EXECUTE') THEN
    RAISE EXCEPTION '000053 execution guard function ACL is invalid' USING ERRCODE='55000';
  END IF;
  IF pg_catalog.has_function_privilege('account_service','account_approval_authorization.validate_legacy_authorization_begin(uuid,text)'::regprocedure,'EXECUTE') THEN
    RAISE EXCEPTION '000053 legacy validator function ACL is invalid' USING ERRCODE='55000';
  END IF;
  SELECT count(*) INTO unexpected
    FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
   WHERE n.nspname='account_approval_authorization'
     AND p.proname IN ('authorization_consumer_mapping_allows','protect_approval_delivery_state',
       'reject_approval_delivery_state_removal','resolve_authorization_binding','validate_legacy_authorization_begin')
     AND (pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
       OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
       OR pg_catalog.has_function_privilege('public',p.oid,'EXECUTE')
       OR pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')
       OR pg_catalog.has_function_privilege('account_approval_writer',p.oid,'EXECUTE')
       OR pg_catalog.has_function_privilege('account_approval_backfill',p.oid,'EXECUTE')
       OR pg_catalog.has_function_privilege('account_approval_relay',p.oid,'EXECUTE'));
  IF unexpected<>0 THEN RAISE EXCEPTION '000053 internal function ACL is invalid' USING ERRCODE='55000'; END IF;
END $$;
RESET ROLE;
SET ROLE account_service;
