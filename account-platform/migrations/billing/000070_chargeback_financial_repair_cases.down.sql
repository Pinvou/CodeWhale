DO $$
BEGIN
    RAISE EXCEPTION 'chargeback financial repair case table is forward-only; retain migration 000070' USING ERRCODE='55000';
END $$;
