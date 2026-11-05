DO $$ BEGIN RAISE EXCEPTION 'direct payment repair commands are forward-only' USING ERRCODE='55000'; END $$;
