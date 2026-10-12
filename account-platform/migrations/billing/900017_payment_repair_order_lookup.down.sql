DO $$ BEGIN RAISE EXCEPTION 'payment repair lookup is forward-only' USING ERRCODE='55000'; END $$;
