DO $$
BEGIN
    RAISE EXCEPTION 'entitlement correction activation v2 is forward-only; retain migration 000057' USING ERRCODE='55000';
END $$;
