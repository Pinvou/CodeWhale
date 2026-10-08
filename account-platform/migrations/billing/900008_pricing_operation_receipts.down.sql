DO $$
BEGIN
 RAISE EXCEPTION 'pricing operation receipts are forward-only; preserve immutable command results';
END $$;
