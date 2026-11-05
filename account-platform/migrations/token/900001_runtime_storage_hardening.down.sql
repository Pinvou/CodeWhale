DO $$ BEGIN RAISE EXCEPTION 'Token runtime storage hardening is forward-only' USING ERRCODE='55000'; END $$;
