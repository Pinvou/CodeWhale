DO $$ BEGIN
    RAISE EXCEPTION 'simulator customer identity facts are forward-only and cannot be discarded';
END $$;
