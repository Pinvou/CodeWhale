DO $$
BEGIN
    RAISE EXCEPTION 'receivable write-off proof claim allowlist is forward-only; retain migration 000064' USING ERRCODE='55000';
END $$;
