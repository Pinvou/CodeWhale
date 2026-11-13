DO $$ BEGIN
 RAISE EXCEPTION 'chargeback cash principal evidence is forward-only' USING ERRCODE='55000';
END $$;
