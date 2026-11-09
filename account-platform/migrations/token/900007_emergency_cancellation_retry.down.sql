DO $$ BEGIN RAISE EXCEPTION 'emergency cancellation retry and review history is forward-only'; END $$;
