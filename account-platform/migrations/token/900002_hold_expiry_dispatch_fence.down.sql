DO $$ BEGIN RAISE EXCEPTION 'permanent cancellation fence is forward only' USING ERRCODE='55000'; END $$;
