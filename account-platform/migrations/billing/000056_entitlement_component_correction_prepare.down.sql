DO $$
BEGIN
    RAISE EXCEPTION 'entitlement correction schema is forward-only; retain migration 000056' USING ERRCODE='55000';
END $$;
