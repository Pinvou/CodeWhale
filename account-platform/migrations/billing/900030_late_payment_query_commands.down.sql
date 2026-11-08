DO $$ BEGIN RAISE EXCEPTION 'late payment query dispatch identity and receipts are forward-only'; END $$;
