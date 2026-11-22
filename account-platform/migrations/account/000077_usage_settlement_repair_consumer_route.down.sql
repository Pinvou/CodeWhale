DO $$
BEGIN
  RAISE EXCEPTION 'usage settlement repair consumer route is forward-only; retain migration 000077' USING ERRCODE='55000';
END $$;
