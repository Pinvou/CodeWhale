-- Append the eight already-registered Billing query-only tuples. The original
-- 000068 migration identity and all existing lease/effect facts remain unchanged.
DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '900009 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=68)
     OR NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=77) THEN
    RAISE EXCEPTION '900009 requires the original query lease and complete Billing consumer routes' USING ERRCODE='55000';
  END IF;
END $$;

CREATE OR REPLACE FUNCTION public.claim_approval_recovery_query(
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
        ('TOKEN_CREDENTIAL_REVOKE',1,'token-command-executor','token-credential-revoke-executor-v1'),
        ('USAGE_SETTLEMENT_REPAIR',1,'billing-command-executor','billing-usage-settlement-repair-executor-p1-v1'),
        ('RECONCILIATION_FINANCIAL_REPAIR',1,'billing-command-executor','billing-reconciliation-financial-repair-executor-p1-v1'),
        ('CHARGEBACK_FINANCIAL_REPAIR',1,'billing-command-executor','billing-chargeback-financial-repair-executor-p1-v1'),
        ('REFUND_EXCEPTION_REPAIR',1,'billing-command-executor','billing-refund-exception-repair-executor-p1-v1'),
        ('PAYMENT_FINANCIAL_REPAIR',1,'billing-command-executor','billing-payment-financial-repair-executor-p1-v1'),
        ('ENTITLEMENT_COMPONENT_ADJUSTMENT',2,'billing-command-executor','billing-entitlement-component-adjustment-executor-p2-v1'),
        ('LEDGER_REPAIR',1,'billing-command-executor','billing-ledger-repair-executor-p1-v1'),
        ('RECEIVABLE_WRITE_OFF',1,'billing-command-executor','billing-receivable-write-off-executor-p1-v1')
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

-- Reassert the existing query-only runtime grant; this does not grant table
-- access, approve a request or authorize another financial domain attempt.
REVOKE ALL ON FUNCTION public.claim_approval_recovery_query(UUID,UUID,UUID,TEXT,BIGINT,BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.claim_approval_recovery_query(UUID,UUID,UUID,TEXT,BIGINT,BIGINT) TO account_approval_recovery;
