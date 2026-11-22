-- Run only by the independently controlled database operator, before 900001.
-- This does not create/reset roles, credentials, data or migration history.
BEGIN;
DO $$ BEGIN
 IF current_user IN ('token_gateway','token_migrator','token_policy_operator','token_schema_owner','token_cap_policy_deployer') THEN
 RAISE EXCEPTION 'Token storage bootstrap requires the independent database operator'; END IF;
 IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('token_gateway','token_migrator','token_policy_operator','token_schema_owner','token_cap_policy_deployer'))<>5
 OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN ('token_gateway','token_migrator','token_policy_operator','token_schema_owner','token_cap_policy_deployer')
 AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls
 OR (rolname IN ('token_schema_owner','token_cap_policy_deployer') AND rolcanlogin)
 OR (rolname IN ('token_migrator','token_policy_operator') AND (NOT rolcanlogin OR rolinherit))))
 OR pg_has_role('token_gateway','token_schema_owner','MEMBER') OR pg_has_role('token_gateway','token_cap_policy_deployer','MEMBER')
 OR NOT pg_has_role('token_migrator','token_gateway','MEMBER') OR NOT pg_has_role('token_migrator','token_schema_owner','MEMBER')
 OR NOT pg_has_role('token_migrator','token_cap_policy_deployer','MEMBER') THEN
 RAISE EXCEPTION 'Token storage bootstrap requires the isolated provisioned roles'; END IF;
 IF to_regclass('public.token_accounts') IS NULL OR to_regclass('public.schema_migrations') IS NULL
 OR (SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE oid=to_regnamespace('public'))<>'token_schema_owner' THEN
 RAISE EXCEPTION 'Token storage bootstrap requires the existing Token database'; END IF;
 IF EXISTS(SELECT 1 FROM pg_roles WHERE (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls) AND pg_has_role('token_gateway',oid,'MEMBER')) THEN
 RAISE EXCEPTION 'Token runtime has unsafe inherited authority'; END IF;
END $$;
GRANT token_gateway TO token_schema_owner WITH INHERIT TRUE,SET TRUE;
COMMIT;
