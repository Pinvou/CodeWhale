DO $$ BEGIN
    RAISE EXCEPTION 'SMS send provider snapshot fence is forward-only'
        USING ERRCODE='55000';
END $$;
