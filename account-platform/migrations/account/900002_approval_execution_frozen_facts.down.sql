DO $$ BEGIN RAISE EXCEPTION 'frozen approval execution facts are forward-only; the guard must not be downgraded' USING ERRCODE='55000'; END $$;
