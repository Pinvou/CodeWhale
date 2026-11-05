DO $$
BEGIN
  RAISE EXCEPTION 'refund exception repair consumer route is forward-only; retain migration 000074' USING ERRCODE='55000';
END $$;
