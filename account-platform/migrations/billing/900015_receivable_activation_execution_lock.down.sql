DO $$ BEGIN RAISE EXCEPTION 'receivable activation lock is forward only' USING ERRCODE='55000'; END $$;
