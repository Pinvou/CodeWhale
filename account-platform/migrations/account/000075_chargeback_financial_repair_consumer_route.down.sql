DO $$
BEGIN
  RAISE EXCEPTION 'chargeback financial repair consumer route is forward-only; retain migration 000075' USING ERRCODE='55000';
END $$;
