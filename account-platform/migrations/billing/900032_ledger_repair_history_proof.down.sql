DO $$ BEGIN
 RAISE EXCEPTION 'ledger repair history proof is forward-only' USING ERRCODE='55000';
END $$;
