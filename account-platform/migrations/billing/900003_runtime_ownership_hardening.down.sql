DO $$ BEGIN RAISE EXCEPTION 'billing ownership hardening is forward-only' USING ERRCODE='55000'; END $$;
