-- Never imply a downgrade of retained stop-loss evidence or its schema.
DO $$ BEGIN
 RAISE EXCEPTION 'provider health recent index is forward-only' USING ERRCODE='55000';
END $$;
