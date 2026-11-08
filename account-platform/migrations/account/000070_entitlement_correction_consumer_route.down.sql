DO $$
BEGIN
  RAISE EXCEPTION 'entitlement correction consumer route is forward-only; retain migration 000070' USING ERRCODE='55000';
END $$;
