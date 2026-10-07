-- Operator-only preparation of one existing Token database. Run before its
-- first public migration; never use a gateway or ordinary migrator credential.
-- No databases, runtime identities or future domain resources are created.
BEGIN;
SELECT pg_advisory_xact_lock(hashtext('pinvou_account_platform_migrations'));
DO $$
DECLARE role_count INTEGER;
BEGIN
    IF current_user IN ('token_gateway','token_migrator','token_policy_operator','token_cap_policy_deployer') THEN
        RAISE EXCEPTION 'Token bootstrap requires the independently controlled database operator';
    END IF;
    SELECT count(*) INTO role_count FROM pg_catalog.pg_roles
    WHERE rolname IN ('token_gateway','token_migrator','token_policy_operator','token_schema_owner','token_cap_policy_deployer');
    IF role_count<>5 OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_roles
        WHERE rolname IN ('token_gateway','token_migrator','token_policy_operator','token_schema_owner','token_cap_policy_deployer')
        AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls
            OR (rolname IN ('token_schema_owner','token_cap_policy_deployer') AND rolcanlogin)
            OR (rolname IN ('token_migrator','token_policy_operator') AND (NOT rolcanlogin OR rolinherit)))
    ) THEN
        RAISE EXCEPTION 'Token bootstrap requires the isolated provisioned deployment roles';
    END IF;
    IF NOT pg_catalog.pg_has_role('token_migrator','token_gateway','MEMBER')
       OR NOT pg_catalog.pg_has_role('token_migrator','token_schema_owner','MEMBER')
       OR NOT pg_catalog.pg_has_role('token_migrator','token_cap_policy_deployer','MEMBER')
       OR pg_catalog.pg_has_role('token_gateway','token_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('token_gateway',(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER') THEN
        RAISE EXCEPTION 'Token runtime and deployment authority must remain separate';
    END IF;
    IF pg_catalog.to_regclass('public.schema_migrations') IS NOT NULL
       OR pg_catalog.to_regclass('public.token_accounts') IS NOT NULL THEN
        RAISE EXCEPTION 'Token bootstrap cannot reset an installed or partial migration history';
    END IF;
END $$;

-- The release migrator must observe cross-session quiescence during the
-- offline governance switch. This does not grant runtime database CREATE.
GRANT pg_read_all_stats TO token_migrator;

-- pgcrypto is trusted, but CREATE EXTENSION still needs database authority.
-- The operator installs it; no database CREATE is lent to runtime/migrator.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
ALTER SCHEMA public OWNER TO token_schema_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE,CREATE ON SCHEMA public TO token_gateway;
COMMIT;
