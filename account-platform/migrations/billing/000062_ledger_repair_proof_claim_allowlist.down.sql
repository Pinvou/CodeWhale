DO $$
BEGIN
    RAISE EXCEPTION 'ledger repair proof claim allowlist is forward-only; retain migration 000062' USING ERRCODE='55000';
END $$;
