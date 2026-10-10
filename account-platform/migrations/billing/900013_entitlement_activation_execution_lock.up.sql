-- Owner-only capability: runtime can lock this one activation, never edit it.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' OR current_user <> 'billing_service'
    OR pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'entitlement activation lock requires isolated deployment identities' USING ERRCODE='42501';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=900003)
    OR NOT EXISTS(SELECT 1 FROM public.billing_approval_command_activations
      WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2) THEN
  RAISE EXCEPTION 'entitlement activation lock requires installed hardening and v2 activation' USING ERRCODE='55000';
 END IF;
END $$;
SET ROLE billing_schema_owner;
CREATE SCHEMA billing_approval_execution AUTHORIZATION billing_schema_owner;
REVOKE ALL ON SCHEMA billing_approval_execution FROM PUBLIC,billing_service;
GRANT USAGE ON SCHEMA billing_approval_execution TO billing_service;
CREATE FUNCTION billing_approval_execution.lock_entitlement_activation()
RETURNS BOOLEAN LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE active BOOLEAN;
BEGIN
 IF session_user <> 'billing_service' OR current_user <> 'billing_schema_owner' THEN
  RAISE EXCEPTION 'entitlement activation lock requires Billing runtime identity' USING ERRCODE='42501';
 END IF;
 SELECT a.state='ACTIVE' INTO active
 FROM public.billing_approval_command_activations a
 WHERE a.command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT'
   AND a.policy_version=2 AND a.snapshot_schema_version=2
 FOR SHARE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'entitlement activation is unavailable' USING ERRCODE='55000';
 END IF;
 RETURN active;
END $$;
REVOKE ALL ON FUNCTION billing_approval_execution.lock_entitlement_activation() FROM PUBLIC,billing_service;
-- Explicitly remove inherited default-privilege grants on these new objects.
-- Leave unrelated owner defaults and all existing objects unchanged.
DO $$ DECLARE recipient RECORD; BEGIN
 FOR recipient IN
  SELECT DISTINCT pg_catalog.pg_get_userbyid(a.grantee) AS role_name
  FROM pg_catalog.pg_namespace n,
       LATERAL pg_catalog.aclexplode(n.nspacl) a
  WHERE n.nspname='billing_approval_execution' AND a.grantee<>0
    AND a.grantee<>(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='billing_schema_owner')
    AND a.grantee<>(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='billing_service') LOOP
  EXECUTE pg_catalog.format('REVOKE ALL ON SCHEMA billing_approval_execution FROM %I',recipient.role_name);
 END LOOP;
 FOR recipient IN
  SELECT DISTINCT pg_catalog.pg_get_userbyid(a.grantee) AS role_name
  FROM pg_catalog.pg_proc p,
       LATERAL pg_catalog.aclexplode(p.proacl) a
  WHERE p.oid='billing_approval_execution.lock_entitlement_activation()'::pg_catalog.regprocedure
    AND a.grantee<>0
    AND a.grantee<>(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='billing_schema_owner') LOOP
  EXECUTE pg_catalog.format('REVOKE ALL ON FUNCTION billing_approval_execution.lock_entitlement_activation() FROM %I',recipient.role_name);
 END LOOP;
END $$;
GRANT EXECUTE ON FUNCTION billing_approval_execution.lock_entitlement_activation() TO billing_service;
SET ROLE billing_service;
