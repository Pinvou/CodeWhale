DO $$
BEGIN
  RAISE EXCEPTION 'reconciliation financial repair consumer route is forward-only; retain migration 000076' USING ERRCODE='55000';
END $$;
