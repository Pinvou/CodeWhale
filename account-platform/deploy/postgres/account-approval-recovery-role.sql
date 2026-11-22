\set ON_ERROR_STOP on
-- Run only against the Account database after a successful full migration verify.
-- No password, role creation, broad membership, or runtime SET ROLE fallback.
\if :{?ACCOUNT_MIGRATIONS_VERIFIED}
\else
  DO $$ BEGIN RAISE EXCEPTION 'explicit Account migration verification is required'; END $$;
\endif
SELECT :'ACCOUNT_MIGRATIONS_VERIFIED' = 'true' AS verified \gset
\if :verified
\else
  DO $$ BEGIN RAISE EXCEPTION 'explicit Account migration verification is required'; END $$;
\endif
BEGIN;
DO $$
DECLARE r pg_catalog.pg_roles%ROWTYPE;
BEGIN
  SELECT * INTO r FROM pg_catalog.pg_roles WHERE rolname='account_approval_recovery';
  IF NOT FOUND OR r.rolinherit OR r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls
     OR EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid OR roleid=r.oid)
     OR EXISTS(SELECT 1 FROM pg_catalog.pg_class WHERE relowner=r.oid)
     OR EXISTS(SELECT 1 FROM pg_catalog.pg_proc WHERE proowner=r.oid) THEN
    RAISE EXCEPTION 'isolated existing recovery role is required';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=68)
     OR NOT has_function_privilege(r.oid,'public.claim_approval_recovery_query(uuid,uuid,uuid,text,bigint,bigint)','EXECUTE')
     OR NOT has_function_privilege(r.oid,'public.release_approval_recovery_query(uuid,uuid,bigint)','EXECUTE')
     OR EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
               WHERE n.nspname IN ('public','account_approval_authorization') AND c.relkind IN ('r','p','v','m')
                 AND has_table_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) THEN
    RAISE EXCEPTION 'reviewed recovery lease privileges and verified Account schema are required';
  END IF;
  ALTER ROLE account_approval_recovery LOGIN;
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO account_approval_recovery',current_database());
END $$;
COMMIT;
