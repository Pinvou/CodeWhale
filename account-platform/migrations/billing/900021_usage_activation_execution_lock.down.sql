DO $$ BEGIN RAISE EXCEPTION 'Usage activation execution lock is forward only' USING ERRCODE='55000'; END $$;
