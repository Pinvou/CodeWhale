DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000073 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=72) THEN
    RAISE EXCEPTION '000073 requires the complete 000072 consumer routing schema' USING ERRCODE='55000';
  END IF;
END $$;

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
                       'billing-payment-financial-repair-executor-p1-v1')
         AND p_workload='billing-command-executor'
      THEN p_serialization_version='APPROVAL_RFC8785_DECIMAL_V1'
    WHEN p_durable IN ('token-model-lifecycle-executor-v1',
                       'token-catalog-rollback-executor-v1',
                       'token-credential-revoke-executor-v1')
         AND p_workload='token-command-executor'
      THEN p_serialization_version IN ('LEGACY_TYPED_V1','APPROVAL_RFC8785_DECIMAL_V1')
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
      WHEN p_command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND p_policy_version=2
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
