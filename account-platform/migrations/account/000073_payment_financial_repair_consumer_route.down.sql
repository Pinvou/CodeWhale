DO $$
BEGIN
  RAISE EXCEPTION 'payment financial repair consumer route is forward-only; retain migration 000073' USING ERRCODE='55000';
END $$;
