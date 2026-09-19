DO $$
BEGIN
    RAISE EXCEPTION 'usage settlement repair case table is forward-only; retain migration 000074' USING ERRCODE='55000';
END $$;
