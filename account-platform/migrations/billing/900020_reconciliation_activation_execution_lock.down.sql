DO $$ BEGIN RAISE EXCEPTION 'reconciliation activation execution lock is forward-only' USING ERRCODE='55000'; END $$;
