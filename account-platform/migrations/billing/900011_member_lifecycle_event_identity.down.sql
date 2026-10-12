DO $$ BEGIN
    RAISE EXCEPTION 'member lifecycle event identity is forward-only; removing it would invalidate durable membership fencing';
END $$;
