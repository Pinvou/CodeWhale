DO $$ BEGIN RAISE EXCEPTION 'first-delivery authorization metadata is forward-only; retained authority must not be downgraded' USING ERRCODE='55000'; END $$;
