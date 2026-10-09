DO $$
BEGIN
    IF session_user<>'token_migrator' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000032 down requires the dedicated token_migrator identity';
    END IF;
END $$;
SET ROLE token_cap_policy_deployer;
DROP FUNCTION public.lock_token_account_owner_cap_policy();
RESET ROLE;
SET ROLE token_gateway;
