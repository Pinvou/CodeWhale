-- Fixed owner capability for the Receivable write-off v1/s1 execution transaction.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' OR current_user <> 'billing_service'
    OR pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'receivable activation lock requires isolated deployment identities' USING ERRCODE='42501';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=900014)
    OR NOT EXISTS(SELECT 1 FROM public.billing_approval_command_activations
      WHERE command_type='RECEIVABLE_WRITE_OFF' AND policy_version=1 AND snapshot_schema_version=1)
    OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace
      WHERE nspname='billing_approval_execution'
        AND nspowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='billing_schema_owner')) THEN
  RAISE EXCEPTION 'receivable activation lock requires installed owner capability and v1 activation' USING ERRCODE='55000';
 END IF;
END $$;
SET ROLE billing_schema_owner;
REVOKE ALL ON SCHEMA billing_approval_execution FROM PUBLIC,billing_service;
GRANT USAGE ON SCHEMA billing_approval_execution TO billing_service;
CREATE FUNCTION billing_approval_execution.lock_receivable_activation()
RETURNS TABLE(active BOOLEAN,activation_version BIGINT,updated_at TIMESTAMPTZ)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF session_user <> 'billing_service' OR current_user <> 'billing_schema_owner' THEN
  RAISE EXCEPTION 'receivable activation lock requires Billing runtime identity' USING ERRCODE='42501';
 END IF;
 RETURN QUERY SELECT a.state='ACTIVE',a.version,a.updated_at
 FROM public.billing_approval_command_activations a
 WHERE a.command_type='RECEIVABLE_WRITE_OFF'
   AND a.policy_version=1 AND a.snapshot_schema_version=1
 FOR SHARE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'receivable activation is unavailable' USING ERRCODE='55000';
 END IF;
END $$;
REVOKE ALL ON FUNCTION billing_approval_execution.lock_receivable_activation() FROM PUBLIC,billing_service;
-- Normalize only this capability and its existing controlled schema.
-- Never change unrelated owner defaults or the entitlement function ACL.
DO $$ DECLARE recipient RECORD; BEGIN
 FOR recipient IN
  SELECT DISTINCT pg_catalog.pg_get_userbyid(a.grantee) AS role_name
  FROM pg_catalog.pg_namespace n,LATERAL pg_catalog.aclexplode(n.nspacl) a
  WHERE n.nspname='billing_approval_execution' AND a.grantee<>0
    AND a.grantee<>(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='billing_schema_owner')
    AND a.grantee<>(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='billing_service') LOOP
  EXECUTE pg_catalog.format('REVOKE ALL ON SCHEMA billing_approval_execution FROM %I',recipient.role_name);
 END LOOP;
 FOR recipient IN
  SELECT DISTINCT pg_catalog.pg_get_userbyid(a.grantee) AS role_name
  FROM pg_catalog.pg_proc p,LATERAL pg_catalog.aclexplode(p.proacl) a
  WHERE p.oid='billing_approval_execution.lock_receivable_activation()'::pg_catalog.regprocedure
    AND a.grantee<>0
    AND a.grantee<>(SELECT oid FROM pg_catalog.pg_roles WHERE rolname='billing_schema_owner') LOOP
  EXECUTE pg_catalog.format('REVOKE ALL ON FUNCTION billing_approval_execution.lock_receivable_activation() FROM %I',recipient.role_name);
 END LOOP;
END $$;
GRANT EXECUTE ON FUNCTION billing_approval_execution.lock_receivable_activation() TO billing_service;
SET ROLE billing_service;
