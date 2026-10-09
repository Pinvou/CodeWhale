DO $$ BEGIN RAISE EXCEPTION 'financial reconciliation authorization separation is forward-only' USING ERRCODE='55000'; END $$;
