DO $$ BEGIN
 RAISE EXCEPTION 'orphan payment order classification is forward-only' USING ERRCODE='55000';
END $$;
