-- Append-only policy and approved intent remain available for audit/recovery.
DO $$ BEGIN
 RAISE EXCEPTION 'lifecycle policy v3 is forward-only; enter SAFE_ROLLBACK without deleting approval history';
END $$;
