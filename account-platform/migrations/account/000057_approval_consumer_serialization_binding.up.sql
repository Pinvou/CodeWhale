-- Bind each physical authorization consumer to the wire versions it is
-- allowed to decode.  Token's three existing durables intentionally remain
-- dual-version during the in-place compatibility migration; Billing's legacy
-- shared durable and three Q+1 durables are disjoint.
DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000057 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=56)
     OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
                    WHERE n.nspname='account_approval_authorization' AND c.relname='approval_delivery_states' AND c.relkind='r') THEN
    RAISE EXCEPTION '000057 requires the complete 000056 approval consumer schema' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(
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
                       'billing-payment-product-resume-executor-p1-v1')
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
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_consumer_mapping_allows(TEXT,TEXT,TEXT,BIGINT,TEXT,TEXT)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

ALTER TABLE account_approval_authorization.approval_delivery_states
  ADD CONSTRAINT approval_delivery_consumer_serialization_binding CHECK (
    account_approval_authorization.authorization_consumer_mapping_allows(
      durable,executor_identity,command_type,policy_version,executor_identity,serialization_version)
  );

CREATE OR REPLACE FUNCTION account_approval_authorization.resolve_authorization_binding(
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
     AND account_approval_authorization.authorization_consumer_mapping_allows(
       p_durable,p_workload,r.command_type,r.policy_version,p.executor_identity,s.serialization_version)
     AND ((s.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY') AND s.delivery_bytes IS NOT NULL
           AND s.event_digest IS NOT NULL AND s.event_digest=pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex')
           AND account_approval_authorization.authorization_delivery_matches(o.event_id,o.event_type,o.aggregate_id,o.aggregate_version,
               o.payload,o.trace_id,o.occurred_at,o.approval_control_epoch,s.serialization_version,s.delivery_bytes))
       OR (s.provenance='RECONSTRUCTED_AFTER_PUBLISH' AND s.serialization_version='LEGACY_TYPED_V1'
           AND s.delivery_bytes IS NULL AND s.event_digest IS NULL
           AND s.reconstructed_semantic_digest IS NOT NULL AND s.reconstructed_semantic_digest~'^[0-9a-f]{64}$'))
$$;

DO $$
DECLARE mapping_oid OID := to_regprocedure('account_approval_authorization.authorization_consumer_mapping_allows(text,text,text,bigint,text,text)');
        resolver_oid OID := to_regprocedure('account_approval_authorization.resolve_authorization_binding(uuid,text,text)');
BEGIN
  IF mapping_oid IS NULL OR resolver_oid IS NULL
     OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_constraint c
                    WHERE c.conrelid='account_approval_authorization.approval_delivery_states'::regclass
                      AND c.conname='approval_delivery_consumer_serialization_binding' AND c.convalidated)
     OR pg_catalog.has_function_privilege('public',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_service',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_writer',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_backfill',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_relay',mapping_oid,'EXECUTE')
     OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
          OR p.prosecdef OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
           FROM pg_catalog.pg_proc p WHERE p.oid=mapping_oid)
     OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
          OR p.prosecdef OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
           FROM pg_catalog.pg_proc p WHERE p.oid=resolver_oid) THEN
    RAISE EXCEPTION '000057 serialization binding inventory is invalid' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
