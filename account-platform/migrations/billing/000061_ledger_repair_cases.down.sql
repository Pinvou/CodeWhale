DO $$
BEGIN
    RAISE EXCEPTION 'ledger repair cases are forward-only; retain migration 000061' USING ERRCODE='55000';
END $$;
