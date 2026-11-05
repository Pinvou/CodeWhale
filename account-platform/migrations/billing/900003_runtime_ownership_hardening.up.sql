-- Forward-only: keep historical migrations and financial facts unchanged.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' AND NOT (SELECT rolsuper FROM pg_roles WHERE rolname=session_user) THEN
  RAISE EXCEPTION 'billing hardening requires the deployment migrator';
 END IF;
 IF pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'runtime must not inherit or SET the schema owner';
 END IF;
END $$;
SET ROLE billing_schema_owner;
CREATE TABLE public.billing_runtime_hardening (
 singleton BOOLEAN PRIMARY KEY CHECK(singleton), installed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO public.billing_runtime_hardening(singleton) VALUES(true);
CREATE FUNCTION public.reject_billing_truncate() RETURNS trigger
 LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN RAISE EXCEPTION 'billing tables cannot be truncated' USING ERRCODE='55000'; END $$;
-- Invoker-only and inaccessible to runtime. The migrator explicitly enters owner.
CREATE FUNCTION public.harden_billing_runtime_objects() RETURNS void
 LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE object RECORD; qualified TEXT;
BEGIN
 IF current_user <> 'billing_schema_owner' THEN
  RAISE EXCEPTION 'billing hardening requires schema owner';
 END IF;
 ALTER SCHEMA public OWNER TO billing_schema_owner;
 REVOKE CREATE ON SCHEMA public FROM PUBLIC,billing_service;
 GRANT USAGE ON SCHEMA public TO billing_service;
 FOR object IN SELECT c.relname,c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
   WHERE n.nspname='public' AND c.relkind IN ('r','p','S','v','m')
     AND pg_get_userbyid(c.relowner) IN ('billing_service','billing_schema_owner') LOOP
  qualified := format('public.%I',object.relname);
  IF object.relkind='S' THEN
   EXECUTE format('ALTER SEQUENCE %s OWNER TO billing_schema_owner',qualified);
   EXECUTE format('REVOKE ALL ON SEQUENCE %s FROM PUBLIC,billing_service',qualified);
   EXECUTE format('GRANT USAGE,SELECT ON SEQUENCE %s TO billing_service',qualified);
  ELSE
   EXECUTE format('ALTER %s %s OWNER TO billing_schema_owner',CASE object.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'TABLE' END,qualified);
   EXECUTE format('REVOKE ALL ON TABLE %s FROM PUBLIC,billing_service',qualified);
   EXECUTE format('GRANT SELECT ON TABLE %s TO billing_service',qualified);
   IF object.relkind IN ('r','p') THEN
    IF object.relname NOT IN ('schema_migrations','billing_runtime_hardening','billing_approval_command_activations','billing_approval_command_activation_history','billing_approval_proof_claims') THEN
     EXECUTE format('GRANT INSERT,UPDATE,DELETE ON TABLE %s TO billing_service',qualified);
    ELSIF object.relname='billing_approval_proof_claims' THEN
     EXECUTE format('GRANT INSERT ON TABLE %s TO billing_service',qualified);
    END IF;
    EXECUTE format('DROP TRIGGER IF EXISTS billing_no_truncate ON %s',qualified);
    EXECUTE format('CREATE TRIGGER billing_no_truncate BEFORE TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION public.reject_billing_truncate()',qualified);
   END IF;
  END IF;
 END LOOP;
 FOR object IN SELECT p.oid::regprocedure::text AS signature,p.proname,p.prosecdef FROM pg_proc p
   JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'
   AND pg_get_userbyid(p.proowner) IN ('billing_service','billing_schema_owner') LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO billing_schema_owner',object.signature);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC,billing_service',object.signature);
  IF object.prosecdef THEN
   IF object.proname <> 'set_recharge_product_lifecycle' THEN
    RAISE EXCEPTION 'unreviewed billing SECURITY DEFINER function: %',object.proname;
   END IF;
   EXECUTE format('ALTER FUNCTION %s SET search_path=pg_catalog,public,pg_temp',object.signature);
   EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO billing_service',object.signature);
  END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname='public' AND pg_get_userbyid(c.relowner)='billing_service') THEN
  RAISE EXCEPTION 'billing runtime still owns schema objects';
 END IF;
END $$;
REVOKE ALL ON FUNCTION public.harden_billing_runtime_objects(),public.reject_billing_truncate() FROM PUBLIC,billing_service;
SELECT public.harden_billing_runtime_objects();
ALTER TABLE public.entitlement_packages ADD CONSTRAINT entitlement_packages_validity_check CHECK(valid_until>valid_from);
SET ROLE billing_service;
