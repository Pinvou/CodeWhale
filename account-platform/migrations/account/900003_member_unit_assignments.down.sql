DO $$ BEGIN
    RAISE EXCEPTION 'member attachment and invitation scope history is forward-only' USING ERRCODE='55000';
END $$;
