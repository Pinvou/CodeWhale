DO $$ BEGIN
    RAISE EXCEPTION 'Token outbox publication generation is forward-only; removing it would invalidate relay fencing'
        USING ERRCODE = '55000';
END $$;
