DO $$ BEGIN RAISE EXCEPTION 'registration dispatch evidence is forward-only' USING ERRCODE='55000'; END $$;
