DO $$ BEGIN
 RAISE EXCEPTION 'metering deactivation receipts are forward-only; preserve immutable operation evidence';
END $$;
