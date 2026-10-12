DO $$ BEGIN
 RAISE EXCEPTION 'ledger activation execution lock is forward-only' USING ERRCODE='55000';
END $$;
