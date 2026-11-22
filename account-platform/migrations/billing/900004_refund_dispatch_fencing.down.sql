DO $$ BEGIN RAISE EXCEPTION 'refund dispatch fencing is forward-only' USING ERRCODE='55000'; END $$;
