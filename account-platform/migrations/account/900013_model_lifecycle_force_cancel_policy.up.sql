-- New immutable lifecycle contract; historical policy2 and approvals remain intact.
DO $$ BEGIN
 IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
  RAISE EXCEPTION 'lifecycle policy v3 requires the isolated account migration identities';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=900009) OR NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=77) THEN
  RAISE EXCEPTION 'lifecycle policy v3 requires the complete consumer and recovery schema' USING ERRCODE='55000';
 END IF;
END $$;
INSERT INTO approval_command_policies(command_type,policy_version,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at)
VALUES('TOKEN_MODEL_LIFECYCLE_CHANGE',3,'APPROVAL_REQUIRED','platform.models.lifecycle.request','platform.models.lifecycle.approve','GOVERNANCE_OR_NORMAL_STATE_MACHINE',1,true,3,'["model_id","catalog_version","current_lifecycle","target_lifecycle","lifecycle_plan_version","effective_at","existing_session_cutoff_at","replacement_model_id","billing_price_version","route_set_fingerprint","provisioning_set_fingerprint","inflight_request_set_fingerprint","policy_version","force_cancel_inflight"]',INTERVAL '24 hours',INTERVAL '5 minutes','token-command-executor',now());

-- Retain every existing consumer tuple and add only lifecycle v3 on the same
-- physical durable. New v3 has no legacy typed-wire authority.
RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE OR REPLACE FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(
  p_durable TEXT,p_workload TEXT,p_command_type TEXT,p_policy_version BIGINT,p_executor_identity TEXT
) RETURNS BOOLEAN LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
SELECT COALESCE(
 (p_durable='billing-wallet-adjustment-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='WALLET_ADJUSTMENT' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-payment-provider-resume-executor-p2-v1' AND p_workload='billing-command-executor'
    AND p_command_type='PAYMENT_PROVIDER_RESUME' AND p_policy_version=2 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-payment-product-resume-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='PAYMENT_PRODUCT_RESUME' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-entitlement-component-adjustment-executor-p2-v1' AND p_workload='billing-command-executor'
    AND p_command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND p_policy_version=2 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-ledger-repair-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='LEDGER_REPAIR' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
 OR (p_durable='billing-receivable-write-off-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='RECEIVABLE_WRITE_OFF' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
  OR (p_durable='billing-payment-financial-repair-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='PAYMENT_FINANCIAL_REPAIR' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
  OR (p_durable='billing-refund-exception-repair-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='REFUND_EXCEPTION_REPAIR' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
  OR (p_durable='billing-chargeback-financial-repair-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='CHARGEBACK_FINANCIAL_REPAIR' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
  OR (p_durable='billing-reconciliation-financial-repair-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='RECONCILIATION_FINANCIAL_REPAIR' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
  OR (p_durable='billing-usage-settlement-repair-executor-p1-v1' AND p_workload='billing-command-executor'
    AND p_command_type='USAGE_SETTLEMENT_REPAIR' AND p_policy_version=1 AND p_executor_identity='billing-command-executor')
 OR (p_durable='token-model-lifecycle-executor-v1' AND p_workload='token-command-executor'
    AND p_command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND p_policy_version IN (2,3) AND p_executor_identity='token-command-executor')
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

CREATE OR REPLACE FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(
  p_durable TEXT,p_workload TEXT,p_command_type TEXT,p_policy_version BIGINT,p_executor_identity TEXT,p_serialization_version TEXT
) RETURNS BOOLEAN LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
SELECT COALESCE(
  account_approval_authorization.authorization_consumer_mapping_allows(
    p_durable,p_workload,p_command_type,p_policy_version,p_executor_identity)
  AND CASE
    WHEN p_durable='billing-command-executor-v1' AND p_workload='billing-command-executor'
      THEN p_serialization_version='LEGACY_TYPED_V1'
    WHEN p_durable IN ('billing-wallet-adjustment-executor-p1-v1',
                       'billing-payment-provider-resume-executor-p2-v1',
                       'billing-payment-product-resume-executor-p1-v1',
                       'billing-entitlement-component-adjustment-executor-p2-v1',
                       'billing-ledger-repair-executor-p1-v1',
                       'billing-receivable-write-off-executor-p1-v1',
                       'billing-payment-financial-repair-executor-p1-v1',
                       'billing-refund-exception-repair-executor-p1-v1',
                       'billing-chargeback-financial-repair-executor-p1-v1',
                       'billing-reconciliation-financial-repair-executor-p1-v1',
                       'billing-usage-settlement-repair-executor-p1-v1')
         AND p_workload='billing-command-executor'
      THEN p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
    WHEN p_durable IN ('token-model-lifecycle-executor-v1',
                       'token-catalog-rollback-executor-v1',
                       'token-credential-revoke-executor-v1')
         AND p_workload='token-command-executor'
      THEN p_serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')
        AND (p_policy_version<>3 OR p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1')
    ELSE false
  END,false)
$$;

CREATE OR REPLACE FUNCTION account_approval_authorization.authorization_consumer_route_durable(
  p_command_type TEXT,p_policy_version BIGINT,p_executor_identity TEXT,p_serialization_version TEXT
) RETURNS TEXT LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
  SELECT candidate.durable
    FROM (SELECT CASE
      WHEN p_executor_identity='billing-command-executor' AND p_serialization_version='LEGACY_TYPED_V1'
        THEN 'billing-command-executor-v1'
      WHEN p_command_type='WALLET_ADJUSTMENT' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-wallet-adjustment-executor-p1-v1'
      WHEN p_command_type='PAYMENT_PROVIDER_RESUME' AND p_policy_version=2
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-payment-provider-resume-executor-p2-v1'
      WHEN p_command_type='PAYMENT_PRODUCT_RESUME' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-payment-product-resume-executor-p1-v1'
      WHEN p_command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND p_policy_version=2
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-entitlement-component-adjustment-executor-p2-v1'
      WHEN p_command_type='LEDGER_REPAIR' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-ledger-repair-executor-p1-v1'
      WHEN p_command_type='RECEIVABLE_WRITE_OFF' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-receivable-write-off-executor-p1-v1'
      WHEN p_command_type='PAYMENT_FINANCIAL_REPAIR' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-payment-financial-repair-executor-p1-v1'
      WHEN p_command_type='REFUND_EXCEPTION_REPAIR' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-refund-exception-repair-executor-p1-v1'
      WHEN p_command_type='CHARGEBACK_FINANCIAL_REPAIR' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-chargeback-financial-repair-executor-p1-v1'
      WHEN p_command_type='RECONCILIATION_FINANCIAL_REPAIR' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-reconciliation-financial-repair-executor-p1-v1'
      WHEN p_command_type='USAGE_SETTLEMENT_REPAIR' AND p_policy_version=1
           AND p_executor_identity='billing-command-executor' AND p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
        THEN 'billing-usage-settlement-repair-executor-p1-v1'
      WHEN p_command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND p_policy_version IN (2,3)
           AND p_executor_identity='token-command-executor'
           AND p_serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')
        THEN 'token-model-lifecycle-executor-v1'
      WHEN p_command_type='TOKEN_MODEL_CATALOG_ROLLBACK' AND p_policy_version=2
           AND p_executor_identity='token-command-executor'
           AND p_serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')
        THEN 'token-catalog-rollback-executor-v1'
      WHEN p_command_type='TOKEN_CREDENTIAL_REVOKE' AND p_policy_version=1
           AND p_executor_identity='token-command-executor'
           AND p_serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')
        THEN 'token-credential-revoke-executor-v1'
    END AS durable) candidate
   WHERE candidate.durable IS NOT NULL
     AND account_approval_authorization.authorization_consumer_mapping_allows(
       candidate.durable,p_executor_identity,p_command_type,p_policy_version,p_executor_identity,p_serialization_version)
$$;

REVOKE ALL ON FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(TEXT,TEXT,TEXT,BIGINT,TEXT) FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(TEXT,TEXT,TEXT,BIGINT,TEXT,TEXT) FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_consumer_route_durable(TEXT,BIGINT,TEXT,TEXT) FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

RESET ROLE;
SET ROLE account_service;

-- The existing query-only recovery lease gains the same explicit v3 tuple;
-- attempts, lease windows, identities and all other mappings stay unchanged.
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
        ('TOKEN_MODEL_LIFECYCLE_CHANGE',3,'token-command-executor','token-model-lifecycle-executor-v1'),
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
