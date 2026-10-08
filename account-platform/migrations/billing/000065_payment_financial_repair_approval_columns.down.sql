DO $$
BEGIN
    RAISE EXCEPTION 'payment financial repair approval linkage is forward-only; retain migration 000065' USING ERRCODE='55000';
END $$;
