DO $$
BEGIN
    RAISE EXCEPTION 'refund exception repair case table is forward-only; retain migration 000067' USING ERRCODE='55000';
END $$;
