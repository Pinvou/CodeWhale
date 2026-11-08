DO $$ BEGIN
    IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
        RAISE EXCEPTION '900015 down requires account_migrator with MIGRATION_ROLE account_service' USING ERRCODE='42501';
    END IF;
    RAISE EXCEPTION 'payment credit closure fence is forward-only; preserve pending and resolved outcomes' USING ERRCODE='55000';
END $$;
