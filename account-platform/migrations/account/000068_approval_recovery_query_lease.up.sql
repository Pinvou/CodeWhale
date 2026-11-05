DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000068 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=67) THEN
    RAISE EXCEPTION '000068 requires the proof single-attempt fence' USING ERRCODE='55000';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='account_approval_recovery'
     AND NOT rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
     AND NOT rolreplication AND NOT rolbypassrls) THEN
    RAISE EXCEPTION '000068 requires an isolated, initially disabled recovery identity' USING ERRCODE='42501';
  END IF;
END $$;

CREATE TABLE public.approval_recovery_query_leases (
  approval_request_id UUID PRIMARY KEY REFERENCES public.approval_requests(id),
  execution_id UUID NOT NULL REFERENCES public.approval_executions(id),
  original_event_id UUID NOT NULL,
  approval_epoch BIGINT NOT NULL CHECK(approval_epoch>0),
  lease_token UUID NOT NULL,
  lease_until TIMESTAMPTZ NOT NULL,
  next_query_at TIMESTAMPTZ NOT NULL,
  query_count INTEGER NOT NULL CHECK(query_count BETWEEN 1 AND 1000000),
  version BIGINT NOT NULL CHECK(version>0)
);
REVOKE ALL ON public.approval_recovery_query_leases FROM PUBLIC,account_approval_recovery;

-- This grant never authorizes a domain attempt: the only claim is a short
-- read-only query lease bound to the original execution and approval epoch.
CREATE FUNCTION public.claim_approval_recovery_query(
  p_request_id UUID,p_execution_id UUID,p_event_id UUID,p_durable TEXT,p_epoch BIGINT,p_request_version BIGINT
) RETURNS TABLE(lease_token UUID,lease_until TIMESTAMPTZ,query_count INTEGER,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
  v_now TIMESTAMPTZ := transaction_timestamp();
BEGIN
  IF session_user<>'account_approval_recovery' OR p_request_id IS NULL OR p_execution_id IS NULL
     OR p_event_id IS NULL OR p_durable IS NULL OR p_epoch<1 OR p_request_version<1 THEN
    RAISE EXCEPTION 'recovery query lease forbidden' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM public.approval_requests r
    JOIN public.approval_executions e ON e.approval_request_id=r.id AND e.command_type=r.command_type
    WHERE r.id=p_request_id AND r.version=p_request_version AND e.id=p_execution_id
      AND e.authorization_event_id=p_event_id AND e.authorization_durable=p_durable
      AND e.authorization_approval_epoch=p_epoch
      AND e.authorization_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
      AND ((r.status='EXECUTING' AND e.status='EXECUTING')
        OR (r.status='EXECUTION_FAILED' AND e.status='FAILED'))
      AND (r.command_type,r.policy_version,e.executor_identity,p_durable) IN (
        ('WALLET_ADJUSTMENT',1,'billing-command-executor','billing-wallet-adjustment-executor-p1-v1'),
        ('PAYMENT_PROVIDER_RESUME',2,'billing-command-executor','billing-payment-provider-resume-executor-p2-v1'),
        ('PAYMENT_PRODUCT_RESUME',1,'billing-command-executor','billing-payment-product-resume-executor-p1-v1'),
        ('TOKEN_MODEL_LIFECYCLE_CHANGE',2,'token-command-executor','token-model-lifecycle-executor-v1'),
        ('TOKEN_MODEL_CATALOG_ROLLBACK',2,'token-command-executor','token-catalog-rollback-executor-v1'),
        ('TOKEN_CREDENTIAL_REVOKE',1,'token-command-executor','token-credential-revoke-executor-v1')
      )
  ) THEN
    RAISE EXCEPTION 'recovery query binding changed' USING ERRCODE='55000';
  END IF;
  RETURN QUERY INSERT INTO public.approval_recovery_query_leases AS l
    (approval_request_id,execution_id,original_event_id,approval_epoch,lease_token,lease_until,next_query_at,query_count,version)
    VALUES (p_request_id,p_execution_id,p_event_id,p_epoch,pg_catalog.gen_random_uuid(),v_now+interval '15 seconds',v_now,1,1)
  ON CONFLICT (approval_request_id) DO UPDATE
    SET lease_token=pg_catalog.gen_random_uuid(),lease_until=v_now+interval '15 seconds',
        query_count=l.query_count+1,version=l.version+1
    WHERE l.execution_id=p_execution_id AND l.original_event_id=p_event_id AND l.approval_epoch=p_epoch
      AND l.lease_until<=v_now AND l.next_query_at<=v_now AND l.query_count<1000000
  RETURNING l.lease_token,l.lease_until,l.query_count,l.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'recovery query lease unavailable' USING ERRCODE='55000'; END IF;
END $$;

CREATE FUNCTION public.release_approval_recovery_query(
  p_request_id UUID,p_lease_token UUID,p_expected_version BIGINT
) RETURNS TABLE(next_query_at TIMESTAMPTZ,version BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
  v_now TIMESTAMPTZ := transaction_timestamp();
BEGIN
  IF session_user<>'account_approval_recovery' OR p_request_id IS NULL OR p_lease_token IS NULL OR p_expected_version<1 THEN
    RAISE EXCEPTION 'recovery query release forbidden' USING ERRCODE='42501';
  END IF;
  RETURN QUERY UPDATE public.approval_recovery_query_leases l
    SET lease_until=v_now,
        next_query_at=v_now+(pg_catalog.power(2,least(l.query_count,9))::INTEGER * interval '1 second'),
        version=l.version+1
    WHERE l.approval_request_id=p_request_id AND l.lease_token=p_lease_token
      AND l.version=p_expected_version AND l.lease_until>v_now
    RETURNING l.next_query_at,l.version;
  IF NOT FOUND THEN RAISE EXCEPTION 'recovery query lease lost' USING ERRCODE='55000'; END IF;
END $$;

REVOKE ALL ON FUNCTION public.claim_approval_recovery_query(UUID,UUID,UUID,TEXT,BIGINT,BIGINT) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.release_approval_recovery_query(UUID,UUID,BIGINT) FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO account_approval_recovery;
GRANT EXECUTE ON FUNCTION public.claim_approval_recovery_query(UUID,UUID,UUID,TEXT,BIGINT,BIGINT) TO account_approval_recovery;
GRANT EXECUTE ON FUNCTION public.release_approval_recovery_query(UUID,UUID,BIGINT) TO account_approval_recovery;
