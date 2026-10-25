DO $$ BEGIN RAISE EXCEPTION 'late payment archive commands and receipts are forward-only'; END $$;
