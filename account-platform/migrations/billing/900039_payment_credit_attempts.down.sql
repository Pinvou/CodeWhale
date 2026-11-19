DO $$ BEGIN
    RAISE EXCEPTION 'payment credit attempts are forward-only; preserve financial effects and abort tombstones' USING ERRCODE='55000';
END $$;
