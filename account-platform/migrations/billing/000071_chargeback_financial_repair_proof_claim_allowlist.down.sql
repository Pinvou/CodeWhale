DO $$
BEGIN
    RAISE EXCEPTION 'chargeback financial repair proof claim allowlist is forward-only; retain migration 000071' USING ERRCODE='55000';
END $$;
