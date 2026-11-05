DO $$
BEGIN
  RAISE EXCEPTION 'receivable write-off consumer route is forward-only; retain migration 000072' USING ERRCODE='55000';
END $$;
