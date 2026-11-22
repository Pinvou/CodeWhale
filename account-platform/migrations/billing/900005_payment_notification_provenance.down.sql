DO $$ BEGIN RAISE EXCEPTION 'callback provenance is forward-only' USING ERRCODE='55000'; END $$;
