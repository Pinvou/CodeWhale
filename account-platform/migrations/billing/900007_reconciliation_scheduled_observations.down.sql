DO $$ BEGIN RAISE EXCEPTION 'scheduled reconciliation observations are forward-only' USING ERRCODE='55000'; END $$;
