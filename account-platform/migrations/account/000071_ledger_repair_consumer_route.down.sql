DO $$
BEGIN
  RAISE EXCEPTION 'ledger repair consumer route is forward-only; retain migration 000071' USING ERRCODE='55000';
END $$;
