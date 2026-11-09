-- Removing these guards would reopen destructive access to retained evidence.
DO $$ BEGIN
    RAISE EXCEPTION 'organization evidence truncate protection is forward-only'
        USING ERRCODE='55000';
END $$;
