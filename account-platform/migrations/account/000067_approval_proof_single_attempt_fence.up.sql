DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000067 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=66) THEN
    RAISE EXCEPTION '000067 requires the proof retry grant fence' USING ERRCODE='55000';
  END IF;
END $$;

GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
RESET ROLE;
SET ROLE account_approval_authorization_owner;

-- A proof-era delivery that was ever granted cannot become READY again, even
-- if an older writer failed to persist its error code. Preserve its evidence.
UPDATE account_approval_authorization.approval_delivery_states d
SET state='DOMAIN_INFLIGHT',version=d.version+1,updated_at=transaction_timestamp()
WHERE d.serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
  AND d.executor_identity='billing-command-executor'
  AND d.state='READY' AND d.domain_attempt_count>0;

CREATE OR REPLACE FUNCTION public.grant_approval_authorization_domain_attempt(p_event_id UUID,p_durable TEXT,p_workload TEXT,p_expected_version BIGINT)
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
     AND (d.serialization_version='LEGACY_TYPED_V1' OR (d.domain_attempt_count=0 AND d.last_error_code IS NULL))
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization domain attempt CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
