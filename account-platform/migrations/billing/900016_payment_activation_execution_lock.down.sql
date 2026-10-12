DO $$ BEGIN RAISE EXCEPTION 'payment activation capability is forward-only' USING ERRCODE='55000'; END $$;
