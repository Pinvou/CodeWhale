-- GSA-08 review hardening: freeze Begin actions to ADMIN_APPROVAL and keep
-- JetStream sequence values lossless across the complete uint64 domain.
DO $$
DECLARE owner_oid OID := (SELECT oid FROM pg_catalog.pg_roles WHERE rolname='account_approval_authorization_owner');
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' OR owner_oid IS NULL THEN
    RAISE EXCEPTION '000054 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=53)
     OR to_regprocedure('public.validate_approval_authorization_begin(uuid,text,text,uuid,bigint,bigint,text,text,text,text,text,bigint,text)') IS NULL
     OR to_regprocedure('public.observe_approval_authorization_delivery(uuid,text,text,text,text,text,text,bigint,bigint,text,bigint,text,bigint,text)') IS NULL
     OR to_regprocedure('public.record_approval_authorization_dlq_puback(uuid,text,text,bigint,text,bigint)') IS NULL THEN
    RAISE EXCEPTION '000054 requires the complete 000053 consumer schema' USING ERRCODE='55000';
  END IF;
END $$;

-- Remove the table-wide grants introduced by 000053. REVOKE of a table ACL
-- does not remove the column ACLs installed by 000051.
REVOKE SELECT ON public.approval_requests FROM account_approval_authorization_owner;
REVOKE UPDATE(id) ON public.approval_requests FROM account_approval_authorization_owner;
GRANT SELECT(id,status,version,command_type,policy_version,target_resource_type,target_resource_id),UPDATE(version)
  ON public.approval_requests TO account_approval_authorization_owner;
REVOKE SELECT ON public.approval_command_policies FROM account_approval_authorization_owner;
GRANT SELECT(command_type,policy_version,executor_identity)
  ON public.approval_command_policies TO account_approval_authorization_owner;
GRANT SELECT(approval_request_id,command_type,executor_identity,
  authorization_event_id,authorization_durable,authorization_approved_request_version,
  authorization_event_digest,authorization_digest_kind,authorization_serialization_version,
  authorization_provenance,authorization_approval_epoch)
  ON public.approval_executions TO account_approval_authorization_owner;
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;

ALTER TABLE account_approval_authorization.approval_delivery_states
  ALTER COLUMN stream_sequence TYPE NUMERIC USING stream_sequence::NUMERIC,
  ALTER COLUMN dlq_stream_sequence TYPE NUMERIC USING dlq_stream_sequence::NUMERIC,
  ADD CONSTRAINT approval_delivery_stream_sequence_uint64 CHECK (
    stream_sequence=pg_catalog.trunc(stream_sequence)
    AND stream_sequence BETWEEN 1 AND 18446744073709551615
  ),
  ADD CONSTRAINT approval_delivery_dlq_stream_sequence_uint64 CHECK (
    dlq_stream_sequence IS NULL OR (
      dlq_stream_sequence=pg_catalog.trunc(dlq_stream_sequence)
      AND dlq_stream_sequence BETWEEN 1 AND 18446744073709551615
    )
  );

CREATE OR REPLACE FUNCTION account_approval_authorization.authorization_begin_action_allows(
  p_mode TEXT,p_relay_state TEXT,p_wire_version SMALLINT,p_proof_required BOOLEAN,
  p_current_epoch BIGINT,p_event_epoch BIGINT,p_serialization TEXT,p_provenance TEXT,
  p_digest_kind TEXT,p_existing_same_execution BOOLEAN
) RETURNS BOOLEAN LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
SELECT COALESCE(
  p_current_epoch>0 AND p_event_epoch>0 AND
  CASE
    WHEN p_mode='LEGACY' THEN
      p_relay_state IN ('RUNNING','FENCED') AND p_wire_version=1 AND NOT p_proof_required
      AND p_event_epoch=p_current_epoch AND p_serialization='LEGACY_TYPED_V1'
      AND ((p_digest_kind='DELIVERY_BYTES' AND p_provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY'))
        OR (p_digest_kind='LEGACY_SEMANTIC' AND p_provenance='RECONSTRUCTED_AFTER_PUBLISH'))
    WHEN p_mode='QUIESCING' THEN
      p_relay_state IN ('RUNNING','FENCED') AND p_wire_version=1 AND NOT p_proof_required
      AND p_current_epoch>1 AND p_event_epoch=p_current_epoch-1 AND p_serialization='LEGACY_TYPED_V1'
      AND ((p_digest_kind='DELIVERY_BYTES' AND p_provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY'))
        OR (p_digest_kind='LEGACY_SEMANTIC' AND p_provenance='RECONSTRUCTED_AFTER_PUBLISH'))
      AND (p_relay_state='RUNNING' OR p_existing_same_execution)
    WHEN p_mode='PROOF_REQUIRED' THEN
      p_relay_state IN ('RUNNING','FENCED') AND p_wire_version=2 AND p_proof_required
      AND p_event_epoch=p_current_epoch AND p_serialization='APPROVAL_RFC8785_DECIMAL_V1'
      AND p_provenance='FROZEN_AT_APPROVAL' AND p_digest_kind='DELIVERY_BYTES'
    WHEN p_mode='SAFE_ROLLBACK' THEN false
    ELSE false
  END,false)
$$;
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_begin_action_allows(
  TEXT,TEXT,SMALLINT,BOOLEAN,BIGINT,BIGINT,TEXT,TEXT,TEXT,BOOLEAN)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE OR REPLACE FUNCTION public.validate_approval_authorization_begin(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_request_id UUID,p_approved_request_version BIGINT,p_approval_epoch BIGINT,
  p_digest TEXT,p_digest_kind TEXT,p_serialization TEXT,p_provenance TEXT,p_command_type TEXT,p_policy_version BIGINT,p_executor TEXT
) RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
  control_row RECORD; request_row RECORD; outbox_row RECORD; binding RECORD; delivery_row RECORD;
  existing_same_execution BOOLEAN := false;
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
  SELECT EXISTS(
    SELECT 1 FROM public.approval_executions e
     WHERE e.approval_request_id=p_request_id AND e.command_type=p_command_type AND e.executor_identity=p_executor
       AND e.authorization_event_id IS NOT DISTINCT FROM p_event_id
       AND e.authorization_durable IS NOT DISTINCT FROM p_durable
       AND e.authorization_approved_request_version IS NOT DISTINCT FROM p_approved_request_version
       AND e.authorization_event_digest IS NOT DISTINCT FROM p_digest
       AND e.authorization_digest_kind IS NOT DISTINCT FROM p_digest_kind
       AND e.authorization_serialization_version IS NOT DISTINCT FROM p_serialization
       AND e.authorization_provenance IS NOT DISTINCT FROM p_provenance
       AND e.authorization_approval_epoch IS NOT DISTINCT FROM p_approval_epoch
  ) INTO existing_same_execution;
  IF control_row.schema_version<>1
     OR NOT account_approval_authorization.authorization_begin_action_allows(
       control_row.mode,control_row.authorization_relay_state,control_row.approval_wire_version,
       control_row.proof_required,control_row.approval_epoch,p_approval_epoch,p_serialization,
       p_provenance,p_digest_kind,existing_same_execution)
     OR p_request_id IS DISTINCT FROM binding.approval_request_id
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

CREATE OR REPLACE FUNCTION account_approval_authorization.validate_legacy_authorization_begin(
  p_request_id UUID,p_executor TEXT
) RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
  control_row RECORD; request_row RECORD; outbox_row RECORD; legacy_durable TEXT;
  digest_kind TEXT; existing_same_execution BOOLEAN := false;
BEGIN
  IF session_user<>'account_service' OR p_request_id IS NULL
     OR p_executor NOT IN ('billing-command-executor','token-command-executor') THEN
    RAISE EXCEPTION 'legacy authorization begin forbidden' USING ERRCODE='42501';
  END IF;
  SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
  SELECT id,status,command_type,policy_version INTO STRICT request_row
    FROM public.approval_requests WHERE id=p_request_id FOR UPDATE;
  SELECT o.event_id,o.aggregate_version,o.approval_control_epoch,o.payload,o.trace_id,o.occurred_at,
         s.serialization_version,s.provenance,s.delivery_bytes,s.event_digest,s.reconstructed_semantic_digest
    INTO STRICT outbox_row
    FROM public.outbox_events o
    JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
   WHERE o.aggregate_id=p_request_id AND o.event_type='account.approval-command.authorized.v1'
     AND o.published_at IS NOT NULL AND o.dead_lettered_at IS NULL
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
  digest_kind := CASE WHEN outbox_row.provenance='RECONSTRUCTED_AFTER_PUBLISH' THEN 'LEGACY_SEMANTIC' ELSE 'DELIVERY_BYTES' END;
  SELECT EXISTS(
    SELECT 1 FROM public.approval_executions e
     WHERE e.approval_request_id=p_request_id AND e.command_type=request_row.command_type
       AND e.executor_identity=p_executor AND e.authorization_event_id IS NULL
       AND e.authorization_durable IS NULL AND e.authorization_approved_request_version IS NULL
       AND e.authorization_event_digest IS NULL AND e.authorization_digest_kind IS NULL
       AND e.authorization_serialization_version IS NULL AND e.authorization_provenance IS NULL
       AND e.authorization_approval_epoch IS NULL
  ) INTO existing_same_execution;
  IF control_row.schema_version<>1
     OR NOT account_approval_authorization.authorization_begin_action_allows(
       control_row.mode,control_row.authorization_relay_state,control_row.approval_wire_version,
       control_row.proof_required,control_row.approval_epoch,outbox_row.approval_control_epoch,
       outbox_row.serialization_version,outbox_row.provenance,digest_kind,existing_same_execution)
     OR request_row.status NOT IN ('APPROVED','EXECUTION_FAILED','EXECUTING','EXECUTED')
     OR legacy_durable IS NULL
     OR NOT account_approval_authorization.authorization_consumer_mapping_allows(
       legacy_durable,p_executor,request_row.command_type,request_row.policy_version,p_executor)
     OR NOT COALESCE(((outbox_row.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY')
          AND outbox_row.delivery_bytes IS NOT NULL AND outbox_row.event_digest IS NOT NULL
          AND outbox_row.event_digest=pg_catalog.encode(pg_catalog.sha256(outbox_row.delivery_bytes),'hex')
          AND account_approval_authorization.authorization_delivery_matches(
            outbox_row.event_id,'account.approval-command.authorized.v1',p_request_id,outbox_row.aggregate_version,
            outbox_row.payload,outbox_row.trace_id,outbox_row.occurred_at,outbox_row.approval_control_epoch,
            outbox_row.serialization_version,outbox_row.delivery_bytes))
       OR (outbox_row.provenance='RECONSTRUCTED_AFTER_PUBLISH' AND outbox_row.delivery_bytes IS NULL
          AND outbox_row.event_digest IS NULL AND outbox_row.reconstructed_semantic_digest~'^[0-9a-f]{64}$')),false) THEN
    RAISE EXCEPTION 'legacy authorization begin binding mismatch' USING ERRCODE='55000';
  END IF;
END $$;

CREATE FUNCTION public.observe_approval_authorization_delivery(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_digest TEXT,p_digest_kind TEXT,p_serialization TEXT,p_provenance TEXT,
  p_approved_request_version BIGINT,p_approval_epoch BIGINT,p_stream TEXT,p_stream_sequence NUMERIC,
  p_command_type TEXT,p_policy_version BIGINT,p_executor TEXT
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE binding RECORD;
BEGIN
  IF session_user<>'account_service' OR p_stream<>'ACCOUNT_PLATFORM' OR p_stream_sequence IS NULL
     OR p_stream_sequence<>pg_catalog.trunc(p_stream_sequence)
     OR p_stream_sequence NOT BETWEEN 1 AND 18446744073709551615 THEN
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

CREATE FUNCTION public.record_approval_authorization_dlq_puback(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_expected_version BIGINT,p_message_id TEXT,p_stream_sequence NUMERIC
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_workload<>'billing-command-executor'
     OR p_stream_sequence IS NULL OR p_stream_sequence<>pg_catalog.trunc(p_stream_sequence)
     OR p_stream_sequence NOT BETWEEN 1 AND 18446744073709551615
     OR p_message_id IS NULL OR p_message_id<>p_event_id::TEXT||':'||p_durable||':dead-letter'
     OR char_length(p_message_id) NOT BETWEEN 1 AND 240 OR p_message_id~'[[:cntrl:]]' THEN
    RAISE EXCEPTION 'authorization DLQ acknowledgement forbidden' USING ERRCODE='42501';
  END IF;
  RETURN QUERY UPDATE account_approval_authorization.approval_delivery_states d
    SET state='DLQ_PUBLISHED_ACK_PENDING',dlq_message_id=p_message_id,dlq_stream_sequence=p_stream_sequence,
        last_error_code='DOMAIN_ATTEMPTS_EXHAUSTED',updated_at=transaction_timestamp(),version=d.version+1
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.version=p_expected_version
     AND d.executor_identity=p_workload AND d.state='DOMAIN_INFLIGHT' AND d.domain_attempt_count=12
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization DLQ acknowledgement CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

REVOKE ALL ON FUNCTION public.observe_approval_authorization_delivery(
  UUID,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,BIGINT,BIGINT,TEXT,NUMERIC,TEXT,BIGINT,TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.record_approval_authorization_dlq_puback(UUID,TEXT,TEXT,BIGINT,TEXT,NUMERIC) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.observe_approval_authorization_delivery(
  UUID,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,BIGINT,BIGINT,TEXT,NUMERIC,TEXT,BIGINT,TEXT) TO account_service;
GRANT EXECUTE ON FUNCTION public.record_approval_authorization_dlq_puback(UUID,TEXT,TEXT,BIGINT,TEXT,NUMERIC) TO account_service;

RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DO $$
DECLARE unexpected BIGINT;
BEGIN
  IF pg_catalog.has_table_privilege('account_approval_authorization_owner','public.approval_requests'::regclass,'SELECT') THEN
    RAISE EXCEPTION '000054 approval request table SELECT is invalid' USING ERRCODE='55000'; END IF;
  IF pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests'::regclass,'id','UPDATE') THEN
    RAISE EXCEPTION '000054 approval request id UPDATE is invalid' USING ERRCODE='55000'; END IF;
  IF NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests'::regclass,'id','SELECT')
     OR NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_requests'::regclass,'version','UPDATE') THEN
    RAISE EXCEPTION '000054 approval request baseline ACL is invalid' USING ERRCODE='55000'; END IF;
  IF pg_catalog.has_table_privilege('account_approval_authorization_owner','public.approval_command_policies'::regclass,'SELECT')
     OR NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies'::regclass,'command_type','SELECT')
     OR NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies'::regclass,'policy_version','SELECT')
     OR NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies'::regclass,'executor_identity','SELECT')
     OR pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_command_policies'::regclass,'requester_permission','SELECT') THEN
    RAISE EXCEPTION '000054 approval policy ACL is invalid' USING ERRCODE='55000'; END IF;
  IF pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE') THEN
    RAISE EXCEPTION '000054 public schema ACL is invalid' USING ERRCODE='55000'; END IF;
  SELECT count(*) INTO unexpected
    FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
   WHERE (n.nspname='public' AND p.proname IN ('validate_approval_authorization_begin',
          'observe_approval_authorization_delivery','record_approval_authorization_dlq_puback')
          AND (pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner' OR NOT p.prosecdef
            OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
            OR NOT pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')
            OR pg_catalog.has_function_privilege('public',p.oid,'EXECUTE')))
      OR (n.nspname='account_approval_authorization' AND p.proname IN (
          'validate_legacy_authorization_begin','authorization_begin_action_allows')
          AND (pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
            OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
            OR pg_catalog.has_function_privilege('public',p.oid,'EXECUTE')
            OR pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')));
  IF unexpected<>0 THEN RAISE EXCEPTION '000054 final function ACL is invalid' USING ERRCODE='55000'; END IF;
  IF to_regprocedure('public.observe_approval_authorization_delivery(uuid,text,text,text,text,text,text,bigint,bigint,text,numeric,text,bigint,text)') IS NULL
     OR to_regprocedure('public.record_approval_authorization_dlq_puback(uuid,text,text,bigint,text,numeric)') IS NULL THEN
    RAISE EXCEPTION '000054 lossless sequence functions are missing' USING ERRCODE='55000';
  END IF;
END $$;
RESET ROLE;
SET ROLE account_service;
