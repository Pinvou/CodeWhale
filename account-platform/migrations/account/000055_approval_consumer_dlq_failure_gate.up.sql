-- GSA-08 review correction: a Billing DLQ PubAck is legal only after the
-- twelfth domain attempt has a persisted, confirmed failure result.
DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000055 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=54)
     OR to_regprocedure('public.record_approval_authorization_dlq_puback(uuid,text,text,bigint,text,numeric)') IS NULL THEN
    RAISE EXCEPTION '000055 requires the complete 000054 consumer hardening schema' USING ERRCODE='55000';
  END IF;
END $$;

GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
CREATE OR REPLACE FUNCTION public.record_approval_authorization_dlq_puback(
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
        updated_at=transaction_timestamp(),version=d.version+1
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.version=p_expected_version
     AND d.executor_identity=p_workload AND d.state='DOMAIN_INFLIGHT' AND d.domain_attempt_count=12
     AND d.last_error_code IS NOT NULL
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization DLQ acknowledgement CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

REVOKE ALL ON FUNCTION public.record_approval_authorization_dlq_puback(UUID,TEXT,TEXT,BIGINT,TEXT,NUMERIC) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.record_approval_authorization_dlq_puback(UUID,TEXT,TEXT,BIGINT,TEXT,NUMERIC) TO account_service;

RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DO $$
DECLARE function_oid OID := to_regprocedure('public.record_approval_authorization_dlq_puback(uuid,text,text,bigint,text,numeric)');
BEGIN
  IF function_oid IS NULL
     OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
           OR NOT p.prosecdef
           OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
           OR NOT pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('public',p.oid,'EXECUTE')
         FROM pg_catalog.pg_proc p WHERE p.oid=function_oid)
     OR pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE') THEN
    RAISE EXCEPTION '000055 DLQ function ACL is invalid' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
