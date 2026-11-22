DO $$ BEGIN RAISE EXCEPTION 'reconciliation pipeline is forward-only; preserve durable review evidence'; END $$;
