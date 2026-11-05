DO $$ BEGIN RAISE EXCEPTION 'reconciliation retry progress is forward-only; preserve scheduling evidence'; END $$;
