-- Immutable wallet receipts and their command identities survive rollback.
DO $$ BEGIN RAISE EXCEPTION 'wallet receipt history is forward-only'; END $$;
