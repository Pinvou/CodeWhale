DO $$ BEGIN RAISE EXCEPTION 'late payment schedule commands and receipts are forward-only'; END $$;
