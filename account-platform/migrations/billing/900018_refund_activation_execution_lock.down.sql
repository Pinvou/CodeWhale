DO $$ BEGIN RAISE EXCEPTION 'refund activation execution lock is forward-only' USING ERRCODE='55000'; END $$;
