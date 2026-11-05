DO $$ BEGIN
 RAISE EXCEPTION 'entitlement activation execution lock is forward-only' USING ERRCODE='55000';
END $$;
