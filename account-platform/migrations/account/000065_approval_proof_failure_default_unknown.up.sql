DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000065 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=64) THEN
    RAISE EXCEPTION '000065 requires the forward-only approval pagination baseline' USING ERRCODE='55000';
  END IF;
END $$;

GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
RESET ROLE;
SET ROLE account_approval_authorization_owner;

-- A proof-era failure is not a retry authorization. It remains query-only
-- until a per-command recovery worker proves a safe original-effect retry.
-- Preserve the pre-cutover legacy state machine without altering its wire.
CREATE OR REPLACE FUNCTION public.record_approval_authorization_domain_failure(
  p_event_id UUID,p_durable TEXT,p_workload TEXT,p_expected_version BIGINT,p_error_code TEXT
) RETURNS TABLE(domain_attempt_count SMALLINT,state TEXT,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_workload<>'billing-command-executor' OR p_error_code IS NULL
     OR p_error_code!~'^[A-Z][A-Z0-9_]{0,119}$' OR p_error_code~'(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)' THEN
    RAISE EXCEPTION 'authorization domain failure invalid' USING ERRCODE='42501';
  END IF;
  RETURN QUERY UPDATE account_approval_authorization.approval_delivery_states d
    SET state=CASE WHEN d.serialization_version='LEGACY_TYPED_V1' AND d.domain_attempt_count<12
                     THEN 'READY' ELSE 'DOMAIN_INFLIGHT' END,
        last_error_code=p_error_code,updated_at=transaction_timestamp(),version=d.version+1
   WHERE d.original_event_id=p_event_id AND d.durable=p_durable AND d.version=p_expected_version
     AND d.state='DOMAIN_INFLIGHT' AND d.domain_attempt_count>0 AND d.executor_identity=p_workload
     AND account_approval_authorization.authorization_consumer_mapping_allows(d.durable,p_workload,d.command_type,d.policy_version,d.executor_identity)
   RETURNING d.domain_attempt_count,d.state,d.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization domain attempt CAS rejected' USING ERRCODE='55000'; END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
