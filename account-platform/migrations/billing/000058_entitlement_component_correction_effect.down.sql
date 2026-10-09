DO $$
BEGIN
    RAISE EXCEPTION 'entitlement correction effect is forward-only; retain migration 000058' USING ERRCODE='55000';
END $$;
