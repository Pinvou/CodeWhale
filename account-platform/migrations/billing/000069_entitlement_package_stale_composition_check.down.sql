DO $$
BEGIN
    RAISE EXCEPTION 'entitlement package composition cleanup is forward-only; retain migration 000069' USING ERRCODE='55000';
END $$;
