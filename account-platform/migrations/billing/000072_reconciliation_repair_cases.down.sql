DO $$
BEGIN
    RAISE EXCEPTION 'reconciliation repair case table is forward-only; retain migration 000072' USING ERRCODE='55000';
END $$;
