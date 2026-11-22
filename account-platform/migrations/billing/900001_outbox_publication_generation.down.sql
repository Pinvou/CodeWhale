DO $$ BEGIN
    RAISE EXCEPTION 'outbox publication generation is forward-only; removing it would invalidate relay fencing';
END $$;
