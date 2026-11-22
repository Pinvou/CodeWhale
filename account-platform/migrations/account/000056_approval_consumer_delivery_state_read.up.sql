-- A consumer must distinguish a twelfth granted-but-unknown attempt from a
-- twelfth confirmed failure before it can publish the authorization DLQ.
DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000056 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=55)
     OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
                    WHERE n.nspname='account_approval_authorization' AND c.relname='approval_delivery_states' AND c.relkind='r') THEN
    RAISE EXCEPTION '000056 requires the complete 000055 approval consumer schema' USING ERRCODE='55000';
  END IF;
END $$;

GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
GRANT SELECT(job_run_id,status,error_class)
  ON public.approval_executions TO account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
CREATE OR REPLACE FUNCTION public.record_approval_authorization_result(
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
       OR (p_workload='token-command-executor' AND d.state='READY')
       OR (p_workload='billing-command-executor' AND d.state='READY' AND EXISTS(
         SELECT 1 FROM public.approval_executions e
          WHERE e.approval_request_id=d.approval_request_id AND e.command_type=d.command_type
            AND e.executor_identity=d.executor_identity AND e.status='EXECUTED'
            AND e.job_run_id=d.original_event_id::TEXT
            AND ((e.authorization_event_id=d.original_event_id AND e.authorization_durable=d.durable
              AND e.authorization_approved_request_version=d.approved_request_version
              AND e.authorization_event_digest=d.event_digest AND e.authorization_digest_kind=d.digest_kind
              AND e.authorization_serialization_version=d.serialization_version
              AND e.authorization_provenance=d.provenance AND e.authorization_approval_epoch=d.approval_epoch)
              OR (d.durable='billing-command-executor-v1' AND d.serialization_version='LEGACY_TYPED_V1'
                AND e.authorization_event_id IS NULL AND e.authorization_durable IS NULL
                AND e.authorization_approved_request_version IS NULL AND e.authorization_event_digest IS NULL
                AND e.authorization_digest_kind IS NULL AND e.authorization_serialization_version IS NULL
                AND e.authorization_provenance IS NULL AND e.authorization_approval_epoch IS NULL)))))
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization result CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

CREATE FUNCTION public.read_approval_authorization_delivery_state(
  p_event_id UUID,p_durable TEXT,p_workload TEXT
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT,last_error_code TEXT,execution_status TEXT,execution_error_class TEXT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_event_id IS NULL OR p_durable IS NULL OR p_workload IS NULL
     OR p_durable<>pg_catalog.btrim(p_durable) OR p_workload<>pg_catalog.btrim(p_workload)
     OR p_durable='' OR p_workload NOT IN ('billing-command-executor','token-command-executor')
     OR char_length(p_durable)>120 OR p_durable~'[[:cntrl:]]' THEN
    RAISE EXCEPTION 'authorization delivery state read forbidden' USING ERRCODE='42501';
  END IF;
  RETURN QUERY SELECT d.domain_attempt_count,d.state,d.version,d.last_error_code,e.status,e.error_class
    FROM account_approval_authorization.approval_delivery_states d
    LEFT JOIN LATERAL (
      SELECT execution.status,execution.error_class FROM public.approval_executions execution
       WHERE execution.approval_request_id=d.approval_request_id AND execution.command_type=d.command_type
         AND execution.executor_identity=d.executor_identity AND execution.job_run_id=d.original_event_id::TEXT
         AND ((execution.authorization_event_id=d.original_event_id AND execution.authorization_durable=d.durable
           AND execution.authorization_approved_request_version=d.approved_request_version
           AND execution.authorization_event_digest=d.event_digest AND execution.authorization_digest_kind=d.digest_kind
           AND execution.authorization_serialization_version=d.serialization_version
           AND execution.authorization_provenance=d.provenance AND execution.authorization_approval_epoch=d.approval_epoch)
           OR (d.durable='billing-command-executor-v1' AND d.serialization_version='LEGACY_TYPED_V1'
             AND execution.authorization_event_id IS NULL AND execution.authorization_durable IS NULL
             AND execution.authorization_approved_request_version IS NULL AND execution.authorization_event_digest IS NULL
             AND execution.authorization_digest_kind IS NULL AND execution.authorization_serialization_version IS NULL
             AND execution.authorization_provenance IS NULL AND execution.authorization_approval_epoch IS NULL))
    ) e ON TRUE
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.executor_identity=p_workload
     AND account_approval_authorization.authorization_consumer_mapping_allows(
       d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity);
  IF NOT FOUND THEN
    RAISE EXCEPTION 'authorization delivery state is unavailable' USING ERRCODE='55000';
  END IF;
END $$;

REVOKE ALL ON FUNCTION public.read_approval_authorization_delivery_state(UUID,TEXT,TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.read_approval_authorization_delivery_state(UUID,TEXT,TEXT) TO account_service;

RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DO $$
DECLARE function_oid OID := to_regprocedure('public.read_approval_authorization_delivery_state(uuid,text,text)');
        result_oid OID := to_regprocedure('public.record_approval_authorization_result(uuid,text,text,bigint)');
BEGIN
  IF function_oid IS NULL
     OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
           OR NOT p.prosecdef
           OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
           OR NOT pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('public',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('account_approval_writer',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('account_approval_backfill',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('account_approval_relay',p.oid,'EXECUTE')
         FROM pg_catalog.pg_proc p WHERE p.oid=function_oid)
     OR result_oid IS NULL
     OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
           OR NOT p.prosecdef OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
           OR NOT pg_catalog.has_function_privilege('account_service',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('public',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('account_approval_writer',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('account_approval_backfill',p.oid,'EXECUTE')
           OR pg_catalog.has_function_privilege('account_approval_relay',p.oid,'EXECUTE')
         FROM pg_catalog.pg_proc p WHERE p.oid=result_oid)
     OR pg_catalog.has_table_privilege('account_approval_authorization_owner','public.approval_executions'::regclass,'SELECT')
     OR NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_executions'::regclass,'job_run_id','SELECT')
     OR NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_executions'::regclass,'status','SELECT')
     OR NOT pg_catalog.has_column_privilege('account_approval_authorization_owner','public.approval_executions'::regclass,'error_class','SELECT')
     OR pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE') THEN
    RAISE EXCEPTION '000056 delivery state reader ACL is invalid' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
