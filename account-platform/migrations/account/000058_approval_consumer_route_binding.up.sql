-- Let every authorization durable authenticate a shared-subject message before
-- deciding whether the message belongs to that durable.  This route binding is
-- read-only and deliberately does not grant Observe/Begin/CAS authority.
DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000058 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=57) THEN
    RAISE EXCEPTION '000058 requires the complete 000057 approval consumer schema' USING ERRCODE='55000';
  END IF;
END $$;

GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE FUNCTION account_approval_authorization.authorization_consumer_route_durable(
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
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_consumer_route_durable(TEXT,BIGINT,TEXT,TEXT)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION account_approval_authorization.resolve_authorization_route(p_event_id UUID)
RETURNS TABLE(approval_request_id UUID,approved_request_version BIGINT,approval_epoch BIGINT,
  serialization_version TEXT,provenance TEXT,event_digest TEXT,digest_kind TEXT,
  command_type TEXT,policy_version BIGINT,executor_identity TEXT,canonical_workload TEXT,canonical_durable TEXT)
LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
  SELECT o.aggregate_id,o.aggregate_version,o.approval_control_epoch,s.serialization_version,s.provenance,
      CASE WHEN s.provenance='RECONSTRUCTED_AFTER_PUBLISH' THEN s.reconstructed_semantic_digest ELSE s.event_digest END,
      CASE WHEN s.provenance='RECONSTRUCTED_AFTER_PUBLISH' THEN 'LEGACY_SEMANTIC' ELSE 'DELIVERY_BYTES' END,
      r.command_type,r.policy_version,p.executor_identity,
      p.executor_identity,
      account_approval_authorization.authorization_consumer_route_durable(
        r.command_type,r.policy_version,p.executor_identity,s.serialization_version)
    FROM public.outbox_events o
    JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
    JOIN public.approval_requests r ON r.id=o.aggregate_id
    JOIN public.approval_command_policies p ON p.command_type=r.command_type AND p.policy_version=r.policy_version
   WHERE o.event_id=p_event_id AND o.event_type='account.approval-command.authorized.v1'
     AND o.published_at IS NOT NULL AND o.dead_lettered_at IS NULL
     AND o.aggregate_version>0 AND o.approval_control_epoch>0
     AND NOT EXISTS(SELECT 1 FROM account_approval_authorization.authorization_event_pending x WHERE x.event_id=o.event_id)
     AND NOT EXISTS(SELECT 1 FROM account_approval_authorization.authorization_delivery_pending x WHERE x.event_id=o.event_id)
     AND ((s.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY') AND s.delivery_bytes IS NOT NULL
           AND s.event_digest IS NOT NULL AND s.event_digest=pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex')
           AND account_approval_authorization.authorization_delivery_matches(o.event_id,o.event_type,o.aggregate_id,o.aggregate_version,
               o.payload,o.trace_id,o.occurred_at,o.approval_control_epoch,s.serialization_version,s.delivery_bytes))
       OR (s.provenance='RECONSTRUCTED_AFTER_PUBLISH' AND s.serialization_version='LEGACY_TYPED_V1'
           AND s.delivery_bytes IS NULL AND s.event_digest IS NULL
           AND s.reconstructed_semantic_digest IS NOT NULL AND s.reconstructed_semantic_digest~'^[0-9a-f]{64}$'))
     AND account_approval_authorization.authorization_consumer_route_durable(
       r.command_type,r.policy_version,p.executor_identity,s.serialization_version) IS NOT NULL
$$;
REVOKE ALL ON FUNCTION account_approval_authorization.resolve_authorization_route(UUID)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

CREATE FUNCTION public.resolve_approval_authorization_route(p_event_id UUID,p_caller_workload TEXT)
RETURNS TABLE(approval_request_id UUID,approved_request_version BIGINT,approval_epoch BIGINT,
  serialization_version TEXT,provenance TEXT,event_digest TEXT,digest_kind TEXT,
  command_type TEXT,policy_version BIGINT,executor_identity TEXT,canonical_workload TEXT,canonical_durable TEXT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
  IF session_user<>'account_service' OR p_event_id IS NULL
     OR p_caller_workload NOT IN ('billing-command-executor','token-command-executor') THEN
    RAISE EXCEPTION 'authorization route reader forbidden' USING ERRCODE='42501';
  END IF;
  RETURN QUERY SELECT * FROM account_approval_authorization.resolve_authorization_route(p_event_id);
  IF NOT FOUND THEN RAISE EXCEPTION 'authorization route is unavailable' USING ERRCODE='55000'; END IF;
END $$;
REVOKE ALL ON FUNCTION public.resolve_approval_authorization_route(UUID,TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.resolve_approval_authorization_route(UUID,TEXT) TO account_service;

RESET ROLE;
SET ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;
DO $$
DECLARE mapping_oid OID := to_regprocedure('account_approval_authorization.authorization_consumer_route_durable(text,bigint,text,text)');
        internal_oid OID := to_regprocedure('account_approval_authorization.resolve_authorization_route(uuid)');
        public_oid OID := to_regprocedure('public.resolve_approval_authorization_route(uuid,text)');
BEGIN
  IF mapping_oid IS NULL OR internal_oid IS NULL OR public_oid IS NULL
     OR pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE')
     OR pg_catalog.has_function_privilege('public',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_service',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_writer',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_backfill',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_relay',mapping_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('public',internal_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_service',internal_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_writer',internal_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_backfill',internal_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('account_approval_relay',internal_oid,'EXECUTE')
     OR pg_catalog.has_function_privilege('public',public_oid,'EXECUTE')
     OR NOT pg_catalog.has_function_privilege('account_service',public_oid,'EXECUTE')
     OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
          OR p.prosecdef OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
           FROM pg_catalog.pg_proc p WHERE p.oid=mapping_oid)
     OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
          OR p.prosecdef OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
           FROM pg_catalog.pg_proc p WHERE p.oid=internal_oid)
     OR (SELECT pg_catalog.pg_get_userbyid(p.proowner)<>'account_approval_authorization_owner'
          OR NOT p.prosecdef OR p.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog']::TEXT[]
           FROM pg_catalog.pg_proc p WHERE p.oid=public_oid) THEN
    RAISE EXCEPTION '000058 authorization route inventory is invalid' USING ERRCODE='55000';
  END IF;
END $$;

RESET ROLE;
SET ROLE account_service;
