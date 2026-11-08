-- Preserve closure state and immutable approval evidence during application rollback.
DO $$ BEGIN
 RAISE EXCEPTION 'account closure saga is forward-only; preserve closure and approval evidence' USING ERRCODE='55000';
END $$;
