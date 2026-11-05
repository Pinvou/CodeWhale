DO $$
BEGIN
    RAISE EXCEPTION 'receivable write-off case table is forward-only; retain migration 000063' USING ERRCODE='55000';
END $$;
