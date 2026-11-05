DO $$ BEGIN RAISE EXCEPTION 'product admission originals are retained; rollback requires an independently reviewed data-preserving plan'; END $$;
