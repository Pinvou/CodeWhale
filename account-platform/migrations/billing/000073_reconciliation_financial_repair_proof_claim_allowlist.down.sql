DO $$
BEGIN
    RAISE EXCEPTION 'reconciliation financial repair proof claim allowlist is forward-only; retain migration 000073' USING ERRCODE='55000';
END $$;
