DO $$
BEGIN
    RAISE EXCEPTION 'payment financial repair proof claim allowlist is forward-only; retain migration 000066' USING ERRCODE='55000';
END $$;
