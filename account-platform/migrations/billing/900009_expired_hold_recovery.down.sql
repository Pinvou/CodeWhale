DO $$ BEGIN RAISE EXCEPTION 'expired hold recovery preserves evidence and is forward only' USING ERRCODE='55000'; END $$;
