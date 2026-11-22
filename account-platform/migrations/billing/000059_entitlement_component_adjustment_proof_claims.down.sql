DO $$
BEGIN
    RAISE EXCEPTION 'entitlement correction proof claim allowlist is forward-only; retain migration 000059' USING ERRCODE='55000';
END $$;
