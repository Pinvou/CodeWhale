DO $$ BEGIN RAISE EXCEPTION 'financial review reason migration is forward-only' USING ERRCODE='55000'; END $$;
