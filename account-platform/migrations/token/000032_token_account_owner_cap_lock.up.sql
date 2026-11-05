DO $$
BEGIN
    IF session_user<>'token_migrator' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000032 requires the dedicated token_migrator identity';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_schema_owner' AND NOT rolcanlogin)
       OR pg_catalog.pg_has_role('token_gateway','token_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('token_gateway',(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER')
       OR (SELECT pg_catalog.pg_get_userbyid(nspowner) FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.to_regnamespace('public')) IS DISTINCT FROM 'token_schema_owner' THEN
        RAISE EXCEPTION '000032 requires an isolated NOLOGIN schema owner';
    END IF;
END $$;
SET ROLE token_schema_owner;

-- SELECT FOR SHARE requires table UPDATE when issued directly. Keep the
-- runtime role read-only and expose only this fixed read-and-lock operation.
GRANT CREATE ON SCHEMA public TO token_cap_policy_deployer;
RESET ROLE;
SET ROLE token_cap_policy_deployer;
CREATE OR REPLACE FUNCTION public.lock_token_account_owner_cap_policy()
RETURNS BIGINT
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
    SELECT active_policy_version
    FROM public.token_account_owner_cap_policy_active
    WHERE singleton_key
    FOR SHARE
$$;
REVOKE ALL ON FUNCTION public.lock_token_account_owner_cap_policy() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.lock_token_account_owner_cap_policy() TO token_gateway;
RESET ROLE;
SET ROLE token_schema_owner;
REVOKE CREATE ON SCHEMA public FROM token_cap_policy_deployer;
RESET ROLE;
SET ROLE token_cap_policy_deployer;
REVOKE INSERT,UPDATE,DELETE,TRUNCATE ON public.token_account_owner_cap_policy_revisions,public.token_account_owner_cap_policy_active FROM token_gateway,PUBLIC;
RESET ROLE;
SET ROLE token_gateway;
