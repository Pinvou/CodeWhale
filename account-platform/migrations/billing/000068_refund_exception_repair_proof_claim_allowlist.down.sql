DO $$
BEGIN
    RAISE EXCEPTION 'refund exception repair proof claim allowlist is forward-only; retain migration 000068' USING ERRCODE='55000';
END $$;
