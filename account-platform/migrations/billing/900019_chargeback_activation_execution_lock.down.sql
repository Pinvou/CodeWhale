DO $$ BEGIN RAISE EXCEPTION 'chargeback activation execution lock is forward-only' USING ERRCODE='55000'; END $$;
