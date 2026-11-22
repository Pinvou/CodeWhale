DO $$ BEGIN
 RAISE EXCEPTION 'Account outbox generation upgrade is forward-only; preserve published generations and historical migration identities';
END $$;
