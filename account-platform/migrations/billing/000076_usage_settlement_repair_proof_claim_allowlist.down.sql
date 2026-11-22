DO $$
BEGIN
    RAISE EXCEPTION 'usage settlement repair proof claim allowlist is forward-only; retain migration 000076' USING ERRCODE='55000';
END $$;
