DO $$
BEGIN
    RAISE EXCEPTION 'repair journal/receivable enumeration is forward-only; retain migration 000060' USING ERRCODE='55000';
END $$;
